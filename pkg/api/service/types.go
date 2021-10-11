package service

import (
	"github.com/dapr/go-sdk/service/common"
	"github.com/tkeel-io/core/pkg/logger"
)

var log = logger.NewLogger("core.api.service")

func errResult(out *common.Content, err error) {
	if err != nil {
		out.Data = []byte(err.Error())
	}
}
