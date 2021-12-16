/*
Copyright 2021 The tKeel Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package entities

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/pkg/errors"
	pb "github.com/tkeel-io/core/api/core/v1"
	"github.com/tkeel-io/core/pkg/config"
	"github.com/tkeel-io/core/pkg/logger"
	"github.com/tkeel-io/core/pkg/runtime"
	"github.com/tkeel-io/core/pkg/statem"
	"github.com/tkeel-io/core/pkg/util"
	"github.com/tkeel-io/kit/log"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

const EntityStateName = "core-state"
const SubscriptionPrefix = "core.subsc."
const TQLEtcdPrefix = "core.tql."

type EntityManager struct {
	daprClient   dapr.Client
	etcdClient   *clientv3.Client
	searchClient pb.SearchHTTPServer
	stateManager *runtime.Manager

	lock   sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc
}

func NewEntityManager(ctx context.Context, mgr *runtime.Manager, searchClient pb.SearchHTTPServer) (*EntityManager, error) {
	var (
		err        error
		daprClient dapr.Client
		etcdClient *clientv3.Client
	)

	if daprClient, err = dapr.NewClient(); nil != err {
		return nil, errors.Wrap(err, "create manager failed")
	} else if etcdClient, err = clientv3.New(clientv3.Config{
		Endpoints:   config.GetConfig().Etcd.Address,
		DialTimeout: 3 * time.Second,
	}); nil != err {
		return nil, errors.Wrap(err, "create manager failed")
	}

	ctx, cancel := context.WithCancel(ctx)

	return &EntityManager{
		ctx:          ctx,
		cancel:       cancel,
		stateManager: mgr,
		daprClient:   daprClient,
		etcdClient:   etcdClient,
		searchClient: searchClient,
		lock:         sync.RWMutex{},
	}, nil
}

func (m *EntityManager) Start() error {
	return errors.Wrap(m.stateManager.Start(), "start entity manager")
}

func (m *EntityManager) OnMessage(ctx context.Context, msgCtx statem.MessageContext) {
	m.stateManager.SendMsg(msgCtx)
}

// ------------------------------------APIs-----------------------------.

// CreateEntity create a entity.
func (m *EntityManager) CreateEntity(ctx context.Context, base *statem.Base) (*statem.Base, error) {
	if base.ID == "" {
		base.ID = uuid()
	}

	// 1. 检查 实体 是否已经存在.
	if _, err := m.getEntityFromState(ctx, base); errors.Is(err, ErrEntityNotFound) {
		if nil == err {
			err = ErrEntityAreadyExisted
		}
		log.Error("create entity", zap.Error(err), logger.EntityID(base.ID))
		return nil, err
	}

	// 2. 创建 实体.
	if bytes, err := statem.EncodeBase(base); nil != err {
		log.Error("create entity", zap.Error(err), logger.EntityID(base.ID))
		return nil, errors.Wrap(err, "create entity")
	} else if err = m.daprClient.SaveState(ctx, EntityStateName, base.ID, bytes); nil != err {
		log.Error("create entity", zap.Error(err), logger.EntityID(base.ID))
		return nil, errors.Wrap(err, "create entity")
	}

	// 3. 向实体发送消息，来在某一个节点上拉起实体，执行实体运行时过程.
	m.stateManager.SendMsg(statem.MessageContext{
		Headers: statem.Header{},
		Message: statem.PropertyMessage{
			StateID:  base.ID,
			Operator: "replace",
		},
	})

	return base, nil
}

// DeleteEntity delete an entity from manager.
func (m *EntityManager) DeleteEntity(ctx context.Context, en *statem.Base) (*statem.Base, error) {
	// 1. delete from runtime.
	base, err := m.stateManager.DeleteStateMarchin(ctx, en)
	if nil != err {
		return nil, errors.Wrap(err, "delete entity from runtime")
	}

	// 2. delete from elasticsearch.
	if _, err = m.searchClient.DeleteByID(ctx, &pb.DeleteByIDRequest{Id: en.ID}); nil != err {
		return nil, errors.Wrap(err, "delete entity from es state")
	}

	// 3. delete from state.
	if err = m.daprClient.DeleteState(ctx, EntityStateName, en.ID); nil != err {
		return nil, errors.Wrap(err, "delete entity from state")
	}

	// 4. delete tql from etcd.
	_, err = m.etcdClient.Delete(ctx, util.FormatMapper(en.Type, en.ID, "subscription"), clientv3.WithPrefix())
	if nil != err {
		log.Error("delete entity mapper", zap.Error(err), logger.EntityID(en.ID), zap.Any("mapper", base.Mappers))
		return nil, errors.Wrap(err, "delete entity")
	}

	// 5. log record.
	log.Info("delete entity", logger.EntityID(en.ID), zap.Any("entity", base))

	return base, nil
}

// GetProperties returns statem.Base.
func (m *EntityManager) GetProperties(ctx context.Context, en *statem.Base) (*statem.Base, error) {
	base, err := m.getEntityFromState(ctx, en)
	if nil != err {
		log.Error("GetProperties", zap.Error(err), logger.EntityID(en.ID))
	}
	return base, errors.Wrap(err, "entity GetProperties")
}

func (m *EntityManager) getEntityFromState(ctx context.Context, en *statem.Base) (*statem.Base, error) {
	var base *statem.Base
	data, err := m.daprClient.GetState(ctx, EntityStateName, en.ID)
	if nil != err {
		return nil, errors.Wrap(err, "get entity properties")
	} else if nil == data || len(data.Value) == 0 {
		return nil, ErrEntityNotFound
	} else if base, err = statem.DecodeBase(data.Value); nil != err {
		return nil, errors.Wrap(err, "get entity properties")
	}

	return base, nil
}

// SetProperties set properties into entity.
func (m *EntityManager) SetProperties(ctx context.Context, en *statem.Base) (*statem.Base, error) {
	if err := m.stateManager.SetProperties(ctx, en); nil != err {
		return nil, errors.Wrap(err, "set entity properties")
	}

	base, err := m.getEntityFromState(ctx, en)
	return base, errors.Wrap(err, "set entity properties")
}

func (m *EntityManager) PatchEntity(ctx context.Context, en *statem.Base, patchData []*pb.PatchData) (*statem.Base, error) {
	if err := m.stateManager.PatchEntity(ctx, en, patchData); nil != err {
		return nil, errors.Wrap(err, "patch entity properties")
	}

	base, err := m.getEntityFromState(ctx, en)
	return base, errors.Wrap(err, "patch entity properties")
}

// SetProperties set properties into entity.
func (m *EntityManager) SetConfigs(ctx context.Context, en *statem.Base) (*statem.Base, error) {
	if err := m.stateManager.SetConfigs(ctx, en); nil != err {
		return nil, errors.Wrap(err, "set entity configs")
	}

	base, err := m.getEntityFromState(ctx, en)
	return base, errors.Wrap(err, "set entity configs")
}

// AppendMapper append a mapper into entity.
func (m *EntityManager) AppendMapper(ctx context.Context, en *statem.Base) (*statem.Base, error) {
	// 1. 判断实体是否存在.
	_, err := m.daprClient.GetState(ctx, EntityStateName, en.ID)
	if nil != err {
		log.Error("append mapper", zap.Error(err), logger.EntityID(en.ID))
		return nil, errors.Wrap(err, "get state")
	}

	// 2. 将 mapper 推到 etcd.
	for _, mm := range en.Mappers {
		if _, err = m.etcdClient.Put(ctx, TQLEtcdPrefix+en.ID+mm.Name, mm.TQLString); nil != err {
			log.Error("append mapper", zap.Error(err), logger.EntityID(en.ID), zap.Any("mapper", mm))
			return nil, errors.Wrap(err, "append mapper")
		}
		log.Info("append mapper", logger.EntityID(en.ID), zap.Any("mapper", mm))
	}

	if err = m.stateManager.AppendMapper(ctx, en); nil != err {
		return nil, errors.Wrap(err, "append mapper")
	}

	base, err := m.getEntityFromState(ctx, en)
	return base, errors.Wrap(err, "append mapper")
}

// DeleteMapper delete mapper from entity.
func (m *EntityManager) RemoveMapper(ctx context.Context, en *statem.Base) (*statem.Base, error) {
	// 1. 判断实体是否存在.
	_, err := m.daprClient.GetState(ctx, EntityStateName, en.ID)
	if nil != err {
		return nil, errors.Wrap(err, "get state")
	}

	// 2. 将 mapper 推到 etcd.
	for _, mm := range en.Mappers {
		if _, err = m.etcdClient.Delete(ctx, TQLEtcdPrefix+en.ID+mm.Name); nil != err {
			log.Error("append mapper", zap.Error(err), logger.EntityID(en.ID), zap.Any("mapper", mm))
			return nil, errors.Wrap(err, "append mapper")
		}
		log.Info("append mapper", logger.EntityID(en.ID), zap.Any("mapper", mm))
	}

	if err = m.stateManager.RemoveMapper(ctx, en); nil != err {
		return nil, errors.Wrap(err, "remove mapper")
	}

	base, err := m.getEntityFromState(ctx, en)
	return base, errors.Wrap(err, "remove mapper")
}

// uuid generate an uuid.
func uuid() string {
	uuid := make([]byte, 16)
	if _, err := rand.Read(uuid); err != nil {
		return ""
	}
	// see section 4.1.1.
	uuid[8] = uuid[8]&^0xc0 | 0x80
	// see section 4.1.3.
	uuid[6] = uuid[6]&^0xf0 | 0x40
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:])
}
