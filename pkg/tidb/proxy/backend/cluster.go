// Copyright 2016 The he3proxy Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package backend

import (
	"github.com/pingcap/tidb/server"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/tidb/proxy/config"
	"github.com/pingcap/tidb/proxy/core/errors"
	"github.com/pingcap/tidb/proxy/core/golog"
	"github.com/pingcap/tidb/proxy/util"
)

const (
	Master      = "master"
	Tidb        = "Tidb"
	TidbSplit   = ","
	WeightSplit = "@"

	TiDBForTP = "tp"
	TiDBForAP = "ap"
)

type Cluster struct {
	Cfg config.ClusterConfig
	BackendPools map[string]*Pool
	ProxyNode *Proxy
	DownAfterNoAlive time.Duration

	Online        bool
	MaxCostPerSql int64
}


type Pool struct {
	ReadyLock sync.Mutex
	sync.RWMutex

	Tidbs         []*DB
	LastTidbIndex int
	RoundRobinQ   []int
	TidbsWeights  []float64

	Costs int64
}

type Proxy struct {
	ProxyAsCompute bool
	ProxyCost int64
	IsPureCompute bool
}


func (cluster *Cluster) CheckCluster() {
	//to do
	//1 check connection alive

	for cluster.Online {
		cluster.checkTidbs()
		time.Sleep(16 * time.Second)
	}
}



func (cluster *Cluster) GetTidbConn(cost int64) (*BackendConn, error) {

	indicate := "qps"
	db, err := cluster.GetNextTidb(indicate, cost)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errors.ErrNoTidbDB
	}

	if db.Self {
		return &BackendConn{db: db}, nil
	}

	if db.dbType == BigCost {
		conn, err := db.newConn()
		if err != nil {
			return nil, errors.ErrConnIsNil
		}
		return &BackendConn{conn, db}, nil
	}
	if atomic.LoadInt32(&(db.state)) == Down {
		return nil, errors.ErrTidbDown
	}

	return db.GetConn()
}

func (cluster *Cluster) checkTidbs() {
	if cluster.BackendPools == nil {
		return
	}
	for _, pool := range cluster.BackendPools {
		pool.RLock()
		if pool.Tidbs == nil {
			pool.RUnlock()
			return
		}
		Tidbs := make([]*DB, len(pool.Tidbs))
		copy(Tidbs, pool.Tidbs)
		pool.RUnlock()

		for i := 0; i < len(Tidbs); i++ {
			if Tidbs[i].Self {
				continue
			}
			if err := Tidbs[i].Ping(); err != nil {
				golog.Error("Node", "checkTidb", "Ping", 0, "db.Addr", Tidbs[i].Addr(), "error", err.Error())
			} else {
				if atomic.LoadInt32(&(Tidbs[i].state)) == Down {
					golog.Info("Node", "checkTidb", "Tidb up", 0, "db.Addr", Tidbs[i].Addr())
					pool.UpTidb(Tidbs[i].addr, cluster.Cfg.User, cluster.Cfg.Password)
				}
				Tidbs[i].SetLastPing()
				if atomic.LoadInt32(&(Tidbs[i].state)) != ManualDown {
					atomic.StoreInt32(&(Tidbs[i].state), Up)
				}
				continue
			}

			if int64(cluster.DownAfterNoAlive) > 0 && time.Now().Unix()-Tidbs[i].GetLastPing() > int64(cluster.DownAfterNoAlive/time.Second) {
				golog.Info("Node", "checkTidb", "Tidb down", 0,
					"db.Addr", Tidbs[i].Addr(),
					"Tidb_down_time", int64(cluster.DownAfterNoAlive/time.Second))
				//If can't ping Tidb after DownAfterNoAlive, set Tidb Down
				pool.DownTidb(Tidbs[i].addr, Down)
			}
		}
	}
}

func GetOnePod(podName,namespace string) *v1.Pod {
	pod, err := KubeClient.CoreV1().Pods(namespace).Get(podName,metav1.GetOptions{})
	if err != nil {
		return nil
	}
	if v,ok := pod.Labels["predelete"];ok {
		if v == "true" {
			return nil
		}
	}
	return pod
}

func (cluster *Cluster) AddTidb(addr, tidbType string) error {
	var db *DB
	var weight float64
	var err error
	if len(addr) == 0 {
		return errors.ErrAddressNull
	}
	pool := cluster.BackendPools[tidbType]
	pool.Lock()
	defer pool.Unlock()
	for _, v := range pool.Tidbs {
		if strings.Split(v.addr, WeightSplit)[0] == strings.Split(addr, WeightSplit)[0] {
			return errors.ErrTidbExist
		}
	}
	//lock check pod status,predelete filter
	podArr := strings.Split(addr,".")
	podName := podArr[0]
	podNs := podArr[2]
	nsArr := strings.Split(podNs,":")
	ns := nsArr[0]
	pod := GetOnePod(podName,ns)
	if pod == nil {
		return nil
	}
	addrAndWeight := strings.Split(addr, WeightSplit)
	if len(addrAndWeight) == 2 {
		weight, err = strconv.ParseFloat(addrAndWeight[1], 64)
		if err != nil {
			return err
		}
	} else {
		weight = 1
	}
	pool.TidbsWeights = append(pool.TidbsWeights, weight)
	if addrAndWeight[0] == "self" {
		db = &DB{
			addr:   addrAndWeight[0],
			Self:   true,
		}
		cluster.ProxyNode.ProxyAsCompute = true
	} else if db, err = cluster.OpenDB(addrAndWeight[0], weight); err != nil {
		return err
	}

		db.dbType = tidbType
		pool.Tidbs = append(pool.Tidbs, db)
		pool.InitBalancer()
		return nil
}


func (cluster *Cluster) DeleteTidb(addr string, tidbType string) error {
	pool := cluster.BackendPools[tidbType]
	he3db,err := pool.InitBalancerAfterDeleteTidb(addr)
	if err != nil {
		return err
	} else {
		if he3db == nil {
			return nil
		}
	}
	CanDelete := func() (bool, error) {
		golog.Info("Cluster", "DeleteTidb", "checking using conn num ", 0)

		if he3db.usingConnsCount == 0 {
			return true, nil
		}
		return false, nil
	}

	if err := util.Retry( 1 * time.Second, 30, CanDelete); err != nil {

		golog.Warn("Cluster", "DeleteTidb", "usingconn been killed", 0, "current conn num",he3db.usingConnsCount)
	}

	return nil
}

func (cluster *Pool) InitBalancerAfterDeleteTidb(addr string)(*DB,error) {
	var i int
	cluster.Lock()
	defer cluster.Unlock()
	TidbCount := len(cluster.Tidbs)
	if TidbCount == 0 {
		return nil, errors.ErrNoTidbDB
	}

	var he3db *DB

	for i = 0; i < TidbCount; i++ {
		if cluster.Tidbs[i].addr == addr {
			he3db = cluster.Tidbs[i]
			break
		}
	}

	if i == TidbCount {
		return nil, errors.ErrTidbNotExist
	}
	if TidbCount == 1 {
		cluster.Tidbs = nil
		cluster.TidbsWeights = nil
		cluster.RoundRobinQ = nil
		return nil, nil
	}

	s := make([]*DB, 0, TidbCount-1)
	sw := make([]float64, 0, TidbCount-1)
	for i = 0; i < TidbCount; i++ {
		if cluster.Tidbs[i].addr != addr {
			s = append(s, cluster.Tidbs[i])
			sw = append(sw, cluster.TidbsWeights[i])
		}
	}

	cluster.Tidbs = s
	cluster.TidbsWeights = sw
	cluster.InitBalancer()
	return he3db, nil
}

func (cluster *Cluster) OpenDB(addr string,weight float64) (*DB, error) {
	db, err := Open(addr, cluster.Cfg.User, cluster.Cfg.Password, "", weight)
	return db, err
}

func (cluster *Pool) UpDB(addr, user, passwd string) (*DB, error) {
	weight:=1.0
	for i,db:= range cluster.Tidbs{
		if db.addr==addr{

	weight=cluster.TidbsWeights[i]
		}
	}

	db, err := Open(addr, user, passwd, "", weight)


	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		db.Close()
		atomic.StoreInt32(&(db.state), Down)
		return nil, err
	}
	atomic.StoreInt32(&(db.state), Up)
	return db, nil
}

func (cluster *Pool) UpTidb(addr, user, passwd string) error {
	db, err := cluster.UpDB(addr, user, passwd)
	if err != nil {
		golog.Error("Node", "UpTidb", err.Error(), 0)
	}

	cluster.Lock()
	for k, Tidb := range cluster.Tidbs {
		if Tidb.addr == addr {
			cluster.Tidbs[k] = db
			cluster.Unlock()
			return nil
		}
	}
	cluster.Tidbs = append(cluster.Tidbs, db)
	cluster.Unlock()

	return err
}

func (cluster *Pool) DownTidb(addr string, state int32) error {
	cluster.RLock()
	if cluster.Tidbs == nil {
		cluster.RUnlock()
		return errors.ErrNoTidbDB
	}
	Tidbs := make([]*DB, len(cluster.Tidbs))
	copy(Tidbs, cluster.Tidbs)
	cluster.RUnlock()

	//Tidb is *DB
	for _, Tidb := range Tidbs {
		if Tidb.addr == addr {
			Tidb.Close()
			atomic.StoreInt32(&(Tidb.state), state)
			break
		}
	}
	return nil
}

//TidbStr(127.0.0.1:3306@2,192.168.0.12:3306@3)
func (pool *Pool) ParseTidbs(Tidbs, dbType string, cfg config.ClusterConfig) error {
	var db *DB
	var weight float64
	var err error

	if len(Tidbs) == 0 {
		return nil
	}
	Tidbs = strings.Trim(Tidbs, TidbSplit)
	TidbArray := strings.Split(Tidbs, TidbSplit)
	count := len(TidbArray)

	//parse addr and weight
	for i := 0; i < count; i++ {
		addrAndWeight := strings.Split(TidbArray[i], WeightSplit)
		if len(addrAndWeight) == 2 {
			weight, err = strconv.ParseFloat(addrAndWeight[1], 64)
			if err != nil {
				return err
			}
		} else {
			weight = 1
		}
		pool.TidbsWeights = append(pool.TidbsWeights, weight)
		if addrAndWeight[0] == "self" {
			db = &DB{
				addr:   addrAndWeight[0],
				Self:   true,
			}
		} else {
			if db, err = Open(addrAndWeight[0], cfg.User, cfg.Password, "", weight); err != nil {
				continue
			}
		}

		db.dbType = dbType
		pool.Tidbs = append(pool.Tidbs, db)
	}
	pool.InitBalancer()
	return nil
}
