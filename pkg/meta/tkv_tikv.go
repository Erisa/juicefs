//go:build !notikv
// +build !notikv

/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package meta

import (
	"context"
	"net/url"
	"os"
	"strings"

	plog "github.com/pingcap/log"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/tikv/client-go/v2/config"
	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/tikv"
	"github.com/tikv/client-go/v2/txnkv"
	"github.com/tikv/client-go/v2/txnkv/txnutil"
	"go.uber.org/zap"
)

func init() {
	Register("tikv", newKVMeta)
	drivers["tikv"] = newTikvClient

}

func newTikvClient(addr string) (tkvClient, error) {
	var plvl string // TiKV (PingCap) uses uber-zap logging, make it less verbose
	switch logger.Level {
	case logrus.TraceLevel:
		plvl = "debug"
	case logrus.DebugLevel:
		plvl = "info"
	case logrus.InfoLevel, logrus.WarnLevel:
		plvl = "warn"
	case logrus.ErrorLevel:
		plvl = "error"
	default:
		plvl = "dpanic"
	}
	l, prop, _ := plog.InitLogger(&plog.Config{Level: plvl}, zap.Fields(zap.String("component", "tikv"), zap.Int("pid", os.Getpid())))
	plog.ReplaceGlobals(l, prop)

	tUrl, err := url.Parse("tikv://" + addr)
	if err != nil {
		return nil, err
	}
	config.UpdateGlobal(func(conf *config.Config) {
		q := tUrl.Query()
		conf.Security = config.NewSecurity(
			q.Get("ca"),
			q.Get("cert"),
			q.Get("key"),
			strings.Split(q.Get("verify-cn"), ","))
	})
	client, err := txnkv.NewClient(strings.Split(tUrl.Host, ","))
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimLeft(tUrl.Path, "/")
	return withPrefix(&tikvClient{client.KVStore}, append([]byte(prefix), 0xFD)), nil
}

type tikvTxn struct {
	*tikv.KVTxn
}

func (tx *tikvTxn) get(key []byte) []byte {
	value, err := tx.Get(context.TODO(), key)
	if tikverr.IsErrNotFound(err) {
		return nil
	}
	if err != nil {
		panic(err)
	}
	return value
}

func (tx *tikvTxn) gets(keys ...[]byte) [][]byte {
	ret, err := tx.BatchGet(context.TODO(), keys)
	if err != nil {
		panic(err)
	}
	values := make([][]byte, len(keys))
	for i, key := range keys {
		values[i] = ret[string(key)]
	}
	return values
}

func (tx *tikvTxn) scanRange0(begin, end []byte, limit int, filter func(k, v []byte) bool) map[string][]byte {
	if limit == 0 {
		return nil
	}

	it, err := tx.Iter(begin, end)
	if err != nil {
		panic(err)
	}
	defer it.Close()
	var ret = make(map[string][]byte)
	for it.Valid() {
		key := it.Key()
		value := it.Value()
		if filter == nil || filter(key, value) {
			ret[string(key)] = value
			if limit > 0 {
				if limit--; limit == 0 {
					break
				}
			}
		}
		if err = it.Next(); err != nil {
			panic(err)
		}
	}
	return ret
}

func (tx *tikvTxn) scanRange(begin, end []byte) map[string][]byte {
	return tx.scanRange0(begin, end, -1, nil)
}

func (tx *tikvTxn) scanKeys(prefix []byte) [][]byte {
	it, err := tx.Iter(prefix, nextKey(prefix))
	if err != nil {
		panic(err)
	}
	defer it.Close()
	var ret [][]byte
	for it.Valid() {
		ret = append(ret, it.Key())
		if err = it.Next(); err != nil {
			panic(err)
		}
	}
	return ret
}

func (tx *tikvTxn) scanKeysRange(begin, end []byte, limit int, filter func(k []byte) bool) [][]byte {
	if limit == 0 {
		return nil
	}
	it, err := tx.Iter(begin, end)
	if err != nil {
		panic(err)
	}
	defer it.Close()
	var ret [][]byte
	for it.Valid() {
		key := it.Key()
		if filter == nil || filter(key) {
			ret = append(ret, it.Key())
			if limit > 0 {
				if limit--; limit == 0 {
					break
				}
			}
		}
		if err = it.Next(); err != nil {
			panic(err)
		}
	}
	return ret
}

func (tx *tikvTxn) scanValues(prefix []byte, limit int, filter func(k, v []byte) bool) map[string][]byte {
	return tx.scanRange0(prefix, nextKey(prefix), limit, filter)
}

func (tx *tikvTxn) exist(prefix []byte) bool {
	it, err := tx.Iter(prefix, nextKey(prefix))
	if err != nil {
		panic(err)
	}
	defer it.Close()
	return it.Valid()
}

func (tx *tikvTxn) set(key, value []byte) {
	if err := tx.Set(key, value); err != nil {
		panic(err)
	}
}

func (tx *tikvTxn) append(key []byte, value []byte) []byte {
	new := append(tx.get(key), value...)
	tx.set(key, new)
	return new
}

func (tx *tikvTxn) incrBy(key []byte, value int64) int64 {
	buf := tx.get(key)
	new := parseCounter(buf)
	if value != 0 {
		new += value
		tx.set(key, packCounter(new))
	}
	return new
}

func (tx *tikvTxn) dels(keys ...[]byte) {
	for _, key := range keys {
		if err := tx.Delete(key); err != nil {
			panic(err)
		}
	}
}

type tikvClient struct {
	client *tikv.KVStore
}

func (c *tikvClient) name() string {
	return "tikv"
}

func (c *tikvClient) shouldRetry(err error) bool {
	return strings.Contains(err.Error(), "write conflict") || strings.Contains(err.Error(), "TxnLockNotFound")
}

func (c *tikvClient) txn(f func(*kvTxn) error) (err error) {
	tx, err := c.client.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			fe, ok := r.(error)
			if ok {
				err = fe
			} else {
				err = errors.Errorf("tikv client txn func error: %v", r)
			}
		}
	}()
	if err = f(&kvTxn{&tikvTxn{tx}}); err != nil {
		return err
	}
	if !tx.IsReadOnly() {
		tx.SetEnable1PC(true)
		tx.SetEnableAsyncCommit(true)
		err = tx.Commit(context.Background())
	}
	return err
}

func (c *tikvClient) scan(prefix []byte, handler func(key, value []byte)) error {
	ts, err := c.client.CurrentTimestamp("global")
	if err != nil {
		return err
	}
	snap := c.client.GetSnapshot(ts)
	snap.SetScanBatchSize(10240)
	snap.SetNotFillCache(true)
	snap.SetPriority(txnutil.PriorityLow)
	it, err := snap.Iter(prefix, nextKey(prefix))
	if err != nil {
		return err
	}
	defer it.Close()
	for it.Valid() {
		handler(it.Key(), it.Value())
		if err = it.Next(); err != nil {
			return err
		}
	}
	return nil
}

func (c *tikvClient) reset(prefix []byte) error {
	_, err := c.client.DeleteRange(context.Background(), prefix, nextKey(prefix), 1)
	return err
}

func (c *tikvClient) close() error {
	return c.client.Close()
}
