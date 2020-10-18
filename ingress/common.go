package ingress

import (
	"encoding/json"
	"net/http"

	"github.com/sirupsen/logrus"
)

type RespBody struct {
	Code   int         `json:"code"`
	Data   interface{} `json:"data"`
	ErrMsg string      `json:"err_msg,omitempty"`
}

func RespErr(w http.ResponseWriter, msg string, status int) {
	w.WriteHeader(status)
	b, err := json.Marshal(RespBody{
		Code:   0,
		Data:   nil,
		ErrMsg: msg,
	})
	if err != nil {
		logrus.Errorf("marshall response err: %v", err)
		return
	}
	if _, err := w.Write(b); err != nil {
		logrus.Errorf("write response bytes err: %v", err)
		return
	}
}
