// The MIT License (MIT)
//
// Copyright (c) 2014 wandoulabs
// Copyright (c) 2014 siddontang
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"github.com/blacktear23/go-proxyprotocol"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/errno"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/plugin"
	"github.com/pingcap/tidb/proxy/backend"
	proxyconfig "github.com/pingcap/tidb/proxy/config"
	"github.com/pingcap/tidb/proxy/core/golog"
	"github.com/pingcap/tidb/proxy/scalepb"
	"github.com/pingcap/tidb/session/txninfo"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/dbterror"
	"github.com/pingcap/tidb/util/fastrand"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/sys/linux"
	"github.com/pingcap/tidb/util/timeutil"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	v1 "k8s.io/api/core/v1"
	"math/rand"
	"net"
	"net/http"
	// For pprof
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/user"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var (
	serverPID   int
	osUser      string
	osVersion   string
	runInGoTest bool
)

func init() {
	serverPID = os.Getpid()
	currentUser, err := user.Current()
	if err != nil {
		osUser = ""
	} else {
		osUser = currentUser.Name
	}
	osVersion, err = linux.OSVersion()
	if err != nil {
		osVersion = ""
	}
	runInGoTest = flag.Lookup("test.v") != nil || flag.Lookup("check.v") != nil
}

var (
	errUnknownFieldType        = dbterror.ClassServer.NewStd(errno.ErrUnknownFieldType)
	errInvalidSequence         = dbterror.ClassServer.NewStd(errno.ErrInvalidSequence)
	errInvalidType             = dbterror.ClassServer.NewStd(errno.ErrInvalidType)
	errNotAllowedCommand       = dbterror.ClassServer.NewStd(errno.ErrNotAllowedCommand)
	errAccessDenied            = dbterror.ClassServer.NewStd(errno.ErrAccessDenied)
	errConCount                = dbterror.ClassServer.NewStd(errno.ErrConCount)
	errSecureTransportRequired = dbterror.ClassServer.NewStd(errno.ErrSecureTransportRequired)
	errMultiStatementDisabled  = dbterror.ClassServer.NewStd(errno.ErrMultiStatementDisabled)
	errNewAbortingConnection   = dbterror.ClassServer.NewStd(errno.ErrNewAbortingConnection)
)

// DefaultCapability is the capability of the server when it is created using the default configuration.
// When server is configured with SSL, the server will have extra capabilities compared to DefaultCapability.
const defaultCapability = mysql.ClientLongPassword | mysql.ClientLongFlag |
	mysql.ClientConnectWithDB | mysql.ClientProtocol41 |
	mysql.ClientTransactions | mysql.ClientSecureConnection | mysql.ClientFoundRows |
	mysql.ClientMultiStatements | mysql.ClientMultiResults | mysql.ClientLocalFiles |
	mysql.ClientConnectAtts | mysql.ClientPluginAuth | mysql.ClientInteractive


const DefaultProxySize = "4.0"

// Server is the MySQL protocol server
type Server struct {
	cfg               *config.Config
	tlsConfig         unsafe.Pointer // *tls.Config
	driver            IDriver
	listener          net.Listener
	socket            net.Listener
	rwlock            sync.RWMutex
	concurrentLimiter *TokenLimiter
	clients           map[uint64]*clientConn
	capability        uint32
	dom               *domain.Domain
	globalConnID      util.GlobalConnID

	statusAddr     string
	statusListener net.Listener
	statusServer   *http.Server
	grpcServer     *grpc.Server
	inShutdownMode bool
	//for proxy
	counter    *Counter
	serverless *Serverless
	cluster    *backend.Cluster
}

// ConnectionCount gets current connection count.
func (s *Server) ConnectionCount() int {
	s.rwlock.RLock()
	cnt := len(s.clients)
	s.rwlock.RUnlock()
	return cnt
}

func (s *Server) getToken() *Token {
	start := time.Now()
	tok := s.concurrentLimiter.Get()
	metrics.TokenGauge.Inc()
	// Note that data smaller than one microsecond is ignored, because that case can be viewed as non-block.
	metrics.GetTokenDurationHistogram.Observe(float64(time.Since(start).Nanoseconds() / 1e3))
	return tok
}

func (s *Server) releaseToken(token *Token) {
	s.concurrentLimiter.Put(token)
	metrics.TokenGauge.Dec()
}

// SetDomain use to set the server domain.
func (s *Server) SetDomain(dom *domain.Domain) {
	s.dom = dom
}

// InitGlobalConnID initialize global connection id.
func (s *Server) InitGlobalConnID(serverIDGetter func() uint64) {
	s.globalConnID = util.GlobalConnID{
		ServerIDGetter: serverIDGetter,
		Is64bits:       true,
	}
}

// newConn creates a new *clientConn from a net.Conn.
// It allocates a connection ID and random salt data for authentication.
func (s *Server) newConn(conn net.Conn) *clientConn {
	cc := newClientConn(s)
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetKeepAlive(s.cfg.Performance.TCPKeepAlive); err != nil {
			logutil.BgLogger().Error("failed to set tcp keep alive option", zap.Error(err))
		}
		if err := tcpConn.SetNoDelay(s.cfg.Performance.TCPNoDelay); err != nil {
			logutil.BgLogger().Error("failed to set tcp no delay option", zap.Error(err))
		}
	}
	cc.setConn(conn)
	cc.salt = fastrand.Buf(20)
	return cc
}

// NewServer creates a new Server.
func NewServer(cfg *config.Config, driver IDriver) (*Server, error) {
	s := &Server{
		cfg:               cfg,
		driver:            driver,
		concurrentLimiter: NewTokenLimiter(cfg.TokenLimit),
		clients:           make(map[uint64]*clientConn),
		globalConnID:      util.GlobalConnID{ServerID: 0, Is64bits: true},
		counter: new(Counter),
	}

	if sl, err := parseServerless(s.cfg.Proxycfg, s, s.counter); err != nil {
		return nil, err
	} else {
		s.serverless = sl
	}

	cluster, err := parseCluster(cfg.Proxycfg.Cluster)
	if err != nil {
		golog.Error("Server", "parseCluster", err.Error(), 0)
		return nil, err
	}

	s.cluster = cluster

	setTxnScope()
	tlsConfig, err := util.LoadTLSCertificates(s.cfg.Security.SSLCA, s.cfg.Security.SSLKey, s.cfg.Security.SSLCert)
	if err != nil {
		logutil.BgLogger().Error("secure connection cert/key/ca load fail", zap.Error(err))
	}
	if tlsConfig != nil {
		setSSLVariable(s.cfg.Security.SSLCA, s.cfg.Security.SSLKey, s.cfg.Security.SSLCert)
		atomic.StorePointer(&s.tlsConfig, unsafe.Pointer(tlsConfig))
		logutil.BgLogger().Info("mysql protocol server secure connection is enabled", zap.Bool("client verification enabled", len(variable.GetSysVar("ssl_ca").Value) > 0))
	} else if cfg.Security.RequireSecureTransport {
		return nil, errSecureTransportRequired.FastGenByArgs()
	}

	setSystemTimeZoneVariable()

	s.capability = defaultCapability
	if s.tlsConfig != nil {
		s.capability |= mysql.ClientSSL
	}

	if s.cfg.Host != "" && (s.cfg.Port != 0 || runInGoTest) {
		addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
		tcpProto := "tcp"
		if s.cfg.EnableTCP4Only {
			tcpProto = "tcp4"
		}
		if s.listener, err = net.Listen(tcpProto, addr); err != nil {
			return nil, errors.Trace(err)
		}
		logutil.BgLogger().Info("server is running MySQL protocol", zap.String("addr", addr))
		if runInGoTest && s.cfg.Port == 0 {
			s.cfg.Port = uint(s.listener.Addr().(*net.TCPAddr).Port)
		}
	}

	if s.cfg.Socket != "" {
		if s.socket, err = net.Listen("unix", s.cfg.Socket); err != nil {
			return nil, errors.Trace(err)
		}
		logutil.BgLogger().Info("server is running MySQL protocol", zap.String("socket", s.cfg.Socket))
	}

	if s.socket == nil && s.listener == nil {
		err = errors.New("Server not configured to listen on either -socket or -host and -port")
		return nil, errors.Trace(err)
	}

	if s.cfg.ProxyProtocol.Networks != "" {
		proxyTarget := s.listener
		if proxyTarget == nil {
			proxyTarget = s.socket
		}
		pplistener, err := proxyprotocol.NewListener(proxyTarget, s.cfg.ProxyProtocol.Networks,
			int(s.cfg.ProxyProtocol.HeaderTimeout))
		if err != nil {
			logutil.BgLogger().Error("ProxyProtocol networks parameter invalid")
			return nil, errors.Trace(err)
		}
		if s.listener != nil {
			s.listener = pplistener
			logutil.BgLogger().Info("server is running MySQL protocol (through PROXY protocol)", zap.String("host", s.cfg.Host))
		} else {
			s.socket = pplistener
			logutil.BgLogger().Info("server is running MySQL protocol (through PROXY protocol)", zap.String("socket", s.cfg.Socket))
		}
	}

	if s.cfg.Status.ReportStatus {
		err = s.listenStatusHTTPServer()
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	// Init rand seed for randomBuf()
	rand.Seed(time.Now().UTC().UnixNano())

	variable.RegisterStatistics(s)

	return s, nil
}


func parseServerless(cfg *proxyconfig.Config, server *Server, count *Counter) (*Serverless, error) {
	var sl *Serverless
	sl, err := NewServerless(cfg, server, count)
	if err != nil {
		return nil, err
	}
	//fmt.Printf("%+v",*sl)
	return sl, nil
}

//for proxy

func parseCluster(cfg proxyconfig.ClusterConfig) (*backend.Cluster, error) {
	var err error
	cluster := new(backend.Cluster)
	cluster.Cfg = cfg
	//for test
	cluster.BackendPools = make(map[string]*backend.Pool)
	cluster.BackendPools[backend.TiDBForTP] = &backend.Pool{}
	cluster.BackendPools[backend.TiDBForAP] = &backend.Pool{}
	cluster.ProxyNode = &backend.Proxy{
		ProxyAsCompute: true,
	}
	cluster.DownAfterNoAlive = time.Duration(cfg.DownAfterNoAlive) * time.Second

	var norms = []string{backend.TiDBForTP, backend.TiDBForAP}
	for _, v := range norms {
		var Podlist *v1.PodList

		var Pod *v1.Pod
		var timeCount int
		for {
			timeCount++
			time.Sleep(600 * time.Millisecond)
			if timeCount > 200 {
				golog.Info("server", "NewServer", "wait pod ready more than 120s",0)
				break
			}
			Podlist = &v1.PodList{}
			Podlist.Items = make([]v1.Pod, 0)

			if v == backend.TiDBForTP {
				ProxyPodlist, err := GetProxyPod(cfg.ClusterName, cfg.NameSpace)
				if err != nil || len(ProxyPodlist.Items) == 0 {
					golog.Warn("server", "NewServer", "GetProxyPod fail or null pod",0,"the err is ",err)
					break
				}
				for _, pod := range ProxyPodlist.Items {
					Podlist.Items = append(Podlist.Items, pod)
				}
			}

			NormalPodlist, err := GetPod(cfg.ClusterName, cfg.NameSpace, v)
			if err != nil || len(NormalPodlist.Items) == 0 {
				golog.Warn("server", "NewServer", "GetPod fail or null pod",0,"the err is ",err,"tidbtype is ",v)
				break
			}
			for _, pod := range NormalPodlist.Items {
				Podlist.Items = append(Podlist.Items, pod)
			}

			readyFlag := false
			for _, v := range Podlist.Items {
				golog.Info("Server", "ReadyOrNot", fmt.Sprint("podname is %s", v.Name), 0)
				if IsPodReady(&v) {
					Pod = v.DeepCopy()
					readyFlag = true
					break
				}
			}
			if readyFlag == true {
				break
			}
		}

		if err = dnsCheck(Pod, &cluster.Cfg); err != nil {
			return nil, err
		}
		tidbs := MakeTidbs(Podlist, cfg.NameSpace)
		golog.Info("server", "NewServer", "Server running", 0, "tidbtype is ", v,
			"Podlist string is ----------", tidbs)

		err = cluster.ParseTidbs(tidbs, v, cfg)
		if err != nil {
			return nil, err
		}
	}

	cluster.Online = true
	go cluster.CheckCluster()

	return cluster, nil
}

func dnsCheck(pod *v1.Pod, cfg *proxyconfig.ClusterConfig) error {
	if pod == nil {
		return nil
	}
	DNSTimeout := int64(60)
	tcName := pod.Labels[InstanceLabelKey]
	name := pod.Name + "." + tcName + "-tidb-peer" + "." + pod.Namespace
	dnscheck := fmt.Sprintf(`
      TIMEOUT_READY=%d
      while ( ! nslookup %s || ! mysql -h%s -u%s  -p%s -P4000 --connect-timeout=2 -e "select 1;" )
      do
         # If TIMEOUT_READY is 0 we should never time out and exit
         TIMEOUT_READY=$(( TIMEOUT_READY-1 ))
                     if [ $TIMEOUT_READY -eq 0 ];
           then
               echo "Timed out waiting for DNS entry"
               exit 1
           fi
         sleep 1
      done`, DNSTimeout, name, name, cfg.User, cfg.Password)
	cmd := exec.Command("/bin/sh", "-c", dnscheck)
	err := cmd.Start()
	if err != nil {
		golog.Error("Server","dnscheck",err.Error(), 0)
		return err
	}
	golog.Info("Server", "dnsCheck", "checking tidb headless ", 0)
	err = cmd.Wait()
	return err
}

func MakeTidbs(Podlist *v1.PodList, ns string) string {
	result := ""
	if Podlist == nil {
		return result
	}
	for _, v := range Podlist.Items {
		podname := v.Name
		cpuNum := ""
		for _, v1 := range v.Spec.Containers {
			if v1.Name == "tidb" {
				cpuNum = v1.Resources.Requests.Cpu().String()
			}
		}
		cpuNum = getFloatCpu(cpuNum)
		tcName := v.Labels[InstanceLabelKey]
		if v.Labels[RoleInstanceLabelKey]== "proxy" {
			result = result + "self" + "@" + DefaultProxySize + ","
		} else {
			result = result + podname + "." + tcName + "-tidb-peer" + "." + ns + ":" + TidbPort + "@" + cpuNum + ","
		}

	}
	return result
}

func setSSLVariable(ca, key, cert string) {
	variable.SetSysVar("have_openssl", "YES")
	variable.SetSysVar("have_ssl", "YES")
	variable.SetSysVar("ssl_cert", cert)
	variable.SetSysVar("ssl_key", key)
	variable.SetSysVar("ssl_ca", ca)
}

func setTxnScope() {
	variable.SetSysVar(variable.TiDBTxnScope, func() string {
		if !variable.EnableLocalTxn.Load() {
			return kv.GlobalTxnScope
		}
		if txnScope := config.GetTxnScopeFromConfig(); txnScope == kv.GlobalTxnScope {
			return kv.GlobalTxnScope
		}
		return kv.LocalTxnScope
	}())
}

// Export config-related metrics
func (s *Server) reportConfig() {
	metrics.ConfigStatus.WithLabelValues("token-limit").Set(float64(s.cfg.TokenLimit))
	metrics.ConfigStatus.WithLabelValues("mem-quota-query").Set(float64(s.cfg.MemQuotaQuery))
	metrics.ConfigStatus.WithLabelValues("max-server-connections").Set(float64(s.cfg.MaxServerConnections))
}

// Run runs the server.
func (s *Server) Run() error {
	metrics.ServerEventCounter.WithLabelValues(metrics.EventStart).Inc()
	s.reportConfig()

	// Start HTTP API to report tidb info such as TPS.
	if s.cfg.Status.ReportStatus {
		s.startStatusHTTP()
	}

	//check proxy is pure compute or complex.
	go s.CheckClusterSilence()

	// flush counter
	go s.flushCounter()

	//run serverless
	go s.runserverless()

	// If error should be reported and exit the server it can be sent on this
	// channel. Otherwise end with sending a nil error to signal "done"
	errChan := make(chan error)
	go s.startNetworkListener(s.listener, false, errChan)
	go s.startNetworkListener(s.socket, true, errChan)
	err := <-errChan
	if err != nil {
		return err
	}
	return <-errChan
}

func (s *Server) flushCounter() {
	for {
		s.counter.FlushCounter()
		time.Sleep(1 * time.Second)
	}
}

func (s *Server) runserverless() {
	for {
		s.serverless.CheckServerless()
		time.Sleep(1 * time.Second)
	}
}

func (s *Server) CheckClusterSilence() {
	var count int
	for {
		tppool := s.cluster.BackendPools[backend.TiDBForTP]
		costs := s.cluster.BackendPools[backend.TiDBForTP].Costs + s.cluster.ProxyNode.ProxyCost
		if costs < 10000 && s.counter.OldClientQPS < 100 {
			count += 1
			if count >= 15 {
				if len(tppool.Tidbs) > 1 {
					scaleReq := &scalepb.ScaleRequest{
						Clustername: s.cfg.Proxycfg.Cluster.ClusterName,
						Namespace:   s.cfg.Proxycfg.Cluster.NameSpace,
						Hashrate:    0,
						Scaletype:   backend.TiDBForTP,
					}
					_, err := ScalerClient.ScaleCluster(context.Background(), scaleReq)
					if err != nil {
						fmt.Errorf("fail to scale in all tp tidb node but proxy node: %s", err)
					}
				}
				fmt.Println("proxy is as pure compute node, proxy cost is ", costs, " max cost for one sql is ", s.cluster.MaxCostPerSql, "normal tp cost is ", s.cluster.BackendPools[backend.TiDBForTP].Costs, ", qps is ", s.counter.OldClientQPS)
				count = 0
			}
			//proxy service as a pure tp type compute node, and no need other tp type tidb.
			//if !s.cluster.ProxyNode.ProxyAsCompute {
			//	proxyAddr := "self" + "@" + DefaultProxySize
			//	if err := s.cluster.AddTidb(proxyAddr, backend.TiDBForTP); err != nil {
			//		fmt.Errorf("add proxy into tp pools failed: %s", err)
			//	} else {
			//		fmt.Println("add proxy into tp pools success when proxy as a pure compute node.")
			//	}
			//}

		} else {
			count = 0
			if s.cluster.ProxyNode.ProxyAsCompute && len(tppool.Tidbs) == 1 {
				scaleReq := &scalepb.ScaleRequest{
					Clustername: s.cfg.Proxycfg.Cluster.ClusterName,
					Namespace:   s.cfg.Proxycfg.Cluster.NameSpace,
					Hashrate:    1,
					Scaletype:   backend.TiDBForTP,
				}
				_, err := ScalerClient.ScaleCluster(context.Background(), scaleReq)
				if err != nil {
					fmt.Errorf("fail to scale out tp tidb node from 0 to 1: %s", err)
				}
			}
			fmt.Println("proxy is as complex compute node, proxy cost is", costs, " max cost for one sql is ", s.cluster.MaxCostPerSql, "normal tp cost is ", s.cluster.BackendPools[backend.TiDBForTP].Costs)

		}

		time.Sleep(1 * time.Second)
	}
}

func (s *Server) startNetworkListener(listener net.Listener, isUnixSocket bool, errChan chan error) {
	if listener == nil {
		errChan <- nil
		return
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok {
				if opErr.Err.Error() == "use of closed network connection" {
					if s.inShutdownMode {
						errChan <- nil
					} else {
						errChan <- err
					}
					return
				}
			}

			// If we got PROXY protocol error, we should continue accept.
			if proxyprotocol.IsProxyProtocolError(err) {
				logutil.BgLogger().Error("PROXY protocol failed", zap.Error(err))
				continue
			}

			logutil.BgLogger().Error("accept failed", zap.Error(err))
			errChan <- err
			return
		}

		clientConn := s.newConn(conn)
		if isUnixSocket {
			clientConn.isUnixSocket = true
		}

		err = plugin.ForeachPlugin(plugin.Audit, func(p *plugin.Plugin) error {
			authPlugin := plugin.DeclareAuditManifest(p.Manifest)
			if authPlugin.OnConnectionEvent != nil {
				host, _, err := clientConn.PeerHost("")
				if err != nil {
					logutil.BgLogger().Error("get peer host failed", zap.Error(err))
					terror.Log(clientConn.Close())
					return errors.Trace(err)
				}
				err = authPlugin.OnConnectionEvent(context.Background(), plugin.PreAuth, &variable.ConnectionInfo{Host: host})
				if err != nil {
					logutil.BgLogger().Info("do connection event failed", zap.Error(err))
					terror.Log(clientConn.Close())
					return errors.Trace(err)
				}
			}
			return nil
		})
		if err != nil {
			continue
		}

		if s.dom != nil && s.dom.IsLostConnectionToPD() {
			logutil.BgLogger().Warn("reject connection due to lost connection to PD")
			terror.Log(clientConn.Close())
			continue
		}

		go s.onConn(clientConn)
	}
}

func (s *Server) startShutdown() {
	s.rwlock.RLock()
	logutil.BgLogger().Info("setting tidb-server to report unhealthy (shutting-down)")
	s.inShutdownMode = true
	s.rwlock.RUnlock()
	// give the load balancer a chance to receive a few unhealthy health reports
	// before acquiring the s.rwlock and blocking connections.
	waitTime := time.Duration(s.cfg.GracefulWaitBeforeShutdown) * time.Second
	if waitTime > 0 {
		logutil.BgLogger().Info("waiting for stray connections before starting shutdown process", zap.Duration("waitTime", waitTime))
		time.Sleep(waitTime)
	}
}

// Close closes the server.
func (s *Server) Close() {
	s.startShutdown()
	s.rwlock.Lock() // prevent new connections
	defer s.rwlock.Unlock()

	if s.listener != nil {
		err := s.listener.Close()
		terror.Log(errors.Trace(err))
		s.listener = nil
	}
	if s.socket != nil {
		err := s.socket.Close()
		terror.Log(errors.Trace(err))
		s.socket = nil
	}
	if s.statusServer != nil {
		err := s.statusServer.Close()
		terror.Log(errors.Trace(err))
		s.statusServer = nil
	}
	if s.grpcServer != nil {
		s.grpcServer.Stop()
		s.grpcServer = nil
	}
	metrics.ServerEventCounter.WithLabelValues(metrics.EventClose).Inc()
}

// onConn runs in its own goroutine, handles queries from this connection.
func (s *Server) onConn(conn *clientConn) {
	ctx := logutil.WithConnID(context.Background(), conn.connectionID)
	if err := conn.handshake(ctx); err != nil {
		if plugin.IsEnable(plugin.Audit) && conn.ctx != nil {
			conn.ctx.GetSessionVars().ConnectionInfo = conn.connectInfo()
			err = plugin.ForeachPlugin(plugin.Audit, func(p *plugin.Plugin) error {
				authPlugin := plugin.DeclareAuditManifest(p.Manifest)
				if authPlugin.OnConnectionEvent != nil {
					pluginCtx := context.WithValue(context.Background(), plugin.RejectReasonCtxValue{}, err.Error())
					return authPlugin.OnConnectionEvent(pluginCtx, plugin.Reject, conn.ctx.GetSessionVars().ConnectionInfo)
				}
				return nil
			})
			terror.Log(err)
		}
		// Some keep alive services will send request to TiDB and disconnect immediately.
		// So we only record metrics.
		metrics.HandShakeErrorCounter.Inc()
		err = conn.Close()
		terror.Log(errors.Trace(err))
		return
	}

	logutil.Logger(ctx).Debug("new connection", zap.String("remoteAddr", conn.bufReadConn.RemoteAddr().String()))

	defer func() {
		logutil.Logger(ctx).Debug("connection closed")
	}()
	s.rwlock.Lock()
	s.clients[conn.connectionID] = conn
	connections := len(s.clients)
	s.rwlock.Unlock()
	metrics.ConnGauge.Set(float64(connections))

	sessionVars := conn.ctx.GetSessionVars()
	if plugin.IsEnable(plugin.Audit) {
		sessionVars.ConnectionInfo = conn.connectInfo()
	}
	err := plugin.ForeachPlugin(plugin.Audit, func(p *plugin.Plugin) error {
		authPlugin := plugin.DeclareAuditManifest(p.Manifest)
		if authPlugin.OnConnectionEvent != nil {
			return authPlugin.OnConnectionEvent(context.Background(), plugin.Connected, sessionVars.ConnectionInfo)
		}
		return nil
	})
	if err != nil {
		return
	}
	// conn.ctx.GetSessionVars().StmtCtx.CurrentRole= utilproxy.TIDB

	connectedTime := time.Now()
	conn.Run(ctx)

	err = plugin.ForeachPlugin(plugin.Audit, func(p *plugin.Plugin) error {
		// Audit plugin may be disabled before a conn is created, leading no connectionInfo in sessionVars.
		if sessionVars.ConnectionInfo == nil {
			sessionVars.ConnectionInfo = conn.connectInfo()
		}
		authPlugin := plugin.DeclareAuditManifest(p.Manifest)
		if authPlugin.OnConnectionEvent != nil {
			sessionVars.ConnectionInfo.Duration = float64(time.Since(connectedTime)) / float64(time.Millisecond)
			err := authPlugin.OnConnectionEvent(context.Background(), plugin.Disconnect, sessionVars.ConnectionInfo)
			if err != nil {
				logutil.BgLogger().Warn("do connection event failed", zap.String("plugin", authPlugin.Name), zap.Error(err))
			}
		}
		return nil
	})
	if err != nil {
		return
	}
}

func (cc *clientConn) connectInfo() *variable.ConnectionInfo {
	connType := "Socket"
	if cc.isUnixSocket {
		connType = "UnixSocket"
	} else if cc.tlsConn != nil {
		connType = "SSL/TLS"
	}
	connInfo := &variable.ConnectionInfo{
		ConnectionID:      cc.connectionID,
		ConnectionType:    connType,
		Host:              cc.peerHost,
		ClientIP:          cc.peerHost,
		ClientPort:        cc.peerPort,
		ServerID:          1,
		ServerPort:        int(cc.server.cfg.Port),
		User:              cc.user,
		ServerOSLoginUser: osUser,
		OSVersion:         osVersion,
		ServerVersion:     mysql.TiDBReleaseVersion,
		SSLVersion:        "v1.2.0", // for current go version
		PID:               serverPID,
		DB:                cc.dbname,
	}
	return connInfo
}

func (s *Server) checkConnectionCount() error {
	// When the value of MaxServerConnections is 0, the number of connections is unlimited.
	if int(s.cfg.MaxServerConnections) == 0 {
		return nil
	}

	s.rwlock.RLock()
	conns := len(s.clients)
	s.rwlock.RUnlock()

	if conns >= int(s.cfg.MaxServerConnections) {
		logutil.BgLogger().Error("too many connections",
			zap.Uint32("max connections", s.cfg.MaxServerConnections), zap.Error(errConCount))
		return errConCount
	}
	return nil
}

// ShowProcessList implements the SessionManager interface.
func (s *Server) ShowProcessList() map[uint64]*util.ProcessInfo {
	s.rwlock.RLock()
	defer s.rwlock.RUnlock()
	rs := make(map[uint64]*util.ProcessInfo, len(s.clients))
	for _, client := range s.clients {
		if pi := client.ctx.ShowProcess(); pi != nil {
			rs[pi.ID] = pi
		}
	}
	return rs
}

// ShowTxnList shows all txn info for displaying in `TIDB_TRX`
func (s *Server) ShowTxnList() []*txninfo.TxnInfo {
	s.rwlock.RLock()
	defer s.rwlock.RUnlock()
	rs := make([]*txninfo.TxnInfo, 0, len(s.clients))
	for _, client := range s.clients {
		if client.ctx.Session != nil {
			info := client.ctx.Session.TxnInfo()
			if info != nil {
				rs = append(rs, info)
			}
		}
	}
	return rs
}

// GetProcessInfo implements the SessionManager interface.
func (s *Server) GetProcessInfo(id uint64) (*util.ProcessInfo, bool) {
	s.rwlock.RLock()
	conn, ok := s.clients[id]
	s.rwlock.RUnlock()
	if !ok {
		return &util.ProcessInfo{}, false
	}
	return conn.ctx.ShowProcess(), ok
}

// Kill implements the SessionManager interface.
func (s *Server) Kill(connectionID uint64, query bool) {
	logutil.BgLogger().Info("kill", zap.Uint64("connID", connectionID), zap.Bool("query", query))
	metrics.ServerEventCounter.WithLabelValues(metrics.EventKill).Inc()

	s.rwlock.RLock()
	defer s.rwlock.RUnlock()
	conn, ok := s.clients[connectionID]
	if !ok {
		return
	}

	if !query {
		// Mark the client connection status as WaitShutdown, when clientConn.Run detect
		// this, it will end the dispatch loop and exit.
		atomic.StoreInt32(&conn.status, connStatusWaitShutdown)
	}
	killConn(conn)
}

// UpdateTLSConfig implements the SessionManager interface.
func (s *Server) UpdateTLSConfig(cfg *tls.Config) {
	atomic.StorePointer(&s.tlsConfig, unsafe.Pointer(cfg))
}

func (s *Server) getTLSConfig() *tls.Config {
	return (*tls.Config)(atomic.LoadPointer(&s.tlsConfig))
}

func killConn(conn *clientConn) {
	sessVars := conn.ctx.GetSessionVars()
	atomic.StoreUint32(&sessVars.Killed, 1)
	conn.mu.RLock()
	cancelFunc := conn.mu.cancelFunc
	conn.mu.RUnlock()
	if cancelFunc != nil {
		cancelFunc()
	}
}

// KillAllConnections kills all connections when server is not gracefully shutdown.
func (s *Server) KillAllConnections() {
	logutil.BgLogger().Info("[server] kill all connections.")

	s.rwlock.RLock()
	defer s.rwlock.RUnlock()
	for _, conn := range s.clients {
		atomic.StoreInt32(&conn.status, connStatusShutdown)
		if err := conn.closeWithoutLock(); err != nil {
			terror.Log(err)
		}
		killConn(conn)
	}
}

var gracefulCloseConnectionsTimeout = 15 * time.Second

// TryGracefulDown will try to gracefully close all connection first with timeout. if timeout, will close all connection directly.
func (s *Server) TryGracefulDown() {
	ctx, cancel := context.WithTimeout(context.Background(), gracefulCloseConnectionsTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.GracefulDown(ctx, done)
	}()
	select {
	case <-ctx.Done():
		s.KillAllConnections()
	case <-done:
		return
	}
}

// GracefulDown waits all clients to close.
func (s *Server) GracefulDown(ctx context.Context, done chan struct{}) {
	logutil.Logger(ctx).Info("[server] graceful shutdown.")
	metrics.ServerEventCounter.WithLabelValues(metrics.EventGracefulDown).Inc()

	count := s.ConnectionCount()
	for i := 0; count > 0; i++ {
		s.kickIdleConnection()

		count = s.ConnectionCount()
		if count == 0 {
			break
		}
		// Print information for every 30s.
		if i%30 == 0 {
			logutil.Logger(ctx).Info("graceful shutdown...", zap.Int("conn count", count))
		}
		ticker := time.After(time.Second)
		select {
		case <-ctx.Done():
			return
		case <-ticker:
		}
	}
	close(done)
}

func (s *Server) kickIdleConnection() {
	var conns []*clientConn
	s.rwlock.RLock()
	for _, cc := range s.clients {
		if cc.ShutdownOrNotify() {
			// Shutdowned conn will be closed by us, and notified conn will exist themselves.
			conns = append(conns, cc)
		}
	}
	s.rwlock.RUnlock()

	for _, cc := range conns {
		err := cc.Close()
		if err != nil {
			logutil.BgLogger().Error("close connection", zap.Error(err))
		}
	}
}

// ServerID implements SessionManager interface.
func (s *Server) ServerID() uint64 {
	return s.dom.ServerID()
}

// setSysTimeZoneOnce is used for parallel run tests. When several servers are running,
// only the first will actually do setSystemTimeZoneVariable, thus we can avoid data race.
var setSysTimeZoneOnce = &sync.Once{}

func setSystemTimeZoneVariable() {
	setSysTimeZoneOnce.Do(func() {
		tz, err := timeutil.GetSystemTZ()
		if err != nil {
			logutil.BgLogger().Error(
				"Error getting SystemTZ, use default value instead",
				zap.Error(err),
				zap.String("default system_time_zone", variable.GetSysVar("system_time_zone").Value))
			return
		}
		variable.SetSysVar("system_time_zone", tz)
	})
}
