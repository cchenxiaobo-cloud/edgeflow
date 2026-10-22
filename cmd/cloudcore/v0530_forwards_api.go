// 数据转发出口 API（v0.53.0，spec 0026，G18「数据转发出口」起点）。
//
// 六端点（122→128）：
//
//	GET    /api/v1/forwards                  规则清单（脱敏视图，viewer+）
//	POST   /api/v1/forwards                  创建规则（operator+）
//	GET    /api/v1/forwards/{id}             规则详情（viewer+）
//	PUT    /api/v1/forwards/{id}             部分更新（operator+；nil 字段=保留）
//	DELETE /api/v1/forwards/{id}             删除规则（admin——对齐 v0.46 矩阵）
//	GET    /api/v1/forwards/{id}/deliveries  送达台账（新→旧，viewer+）
//
// RBAC 由全局 middleware 统一管控；规则 Secret 不回显（hasSecret 代替）；
// 投递异步执行（alarmstore 迁移挂点 → Dispatch），本 API 面不提供同步测试投递
// （非目标，KI 登记）。校验/重试/签名语义见 cloud/pkg/forwardstore 包注释。
package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"edgeflow/cloud/pkg/forwardstore"
)

// forwardsBodyLimit 是规则请求体上限（64KB——远超字段限额，防御性兜底）。
const forwardsBodyLimit = 64 * 1024

// registerForwardsAPIs 注册数据转发 6 端点。
func registerForwardsAPIs(mux *http.ServeMux, fs *forwardstore.Store) {
	// ─── 规则清单 ───
	mux.HandleFunc("GET /api/v1/forwards", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"rules": fs.List()})
	})

	// ─── 创建规则 ───
	mux.HandleFunc("POST /api/v1/forwards", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name         string   `json:"name"`
			SinkType     string   `json:"sinkType"` // v0.59.0：webhook（默认）| mqtt
			TargetURL    string   `json:"targetUrl"`
			Topic        string   `json:"topic"` // v0.59.0：mqtt sink 主题（空 = edgeflow/forward/<id>）
			Secret       string   `json:"secret"`
			MqttUsername string   `json:"mqttUsername"` // v0.64.0：mqtt sink 认证
			MqttPassword string   `json:"mqttPassword"` // v0.64.0：mqtt sink 认证
			MqttTLS      bool     `json:"mqttTLS"`      // v0.64.0：TLS 拨号
			Events       []string `json:"events"`
			Enabled      *bool    `json:"enabled"` // 缺省 true
		}
		if err := decodeForwardsBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), nil)
			return
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		rule, err := fs.Create(body.Name, body.SinkType, body.TargetURL, body.Topic, body.Secret, body.MqttUsername, body.MqttPassword, body.MqttTLS, body.Events, enabled, time.Now().UnixMilli())
		if err != nil {
			writeFwdErr(w, err)
			return
		}
		view, _ := fs.Get(rule.ID) // 创建后回读脱敏视图（失败理论上不可达）
		writeJSON(w, http.StatusCreated, view)
	})

	// ─── 规则详情 ───
	mux.HandleFunc("GET /api/v1/forwards/{id}", func(w http.ResponseWriter, r *http.Request) {
		view, ok := fs.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "转发规则不存在", nil)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})

	// ─── 部分更新 ───
	mux.HandleFunc("PUT /api/v1/forwards/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name      *string  `json:"name"`
			SinkType  *string  `json:"sinkType"` // webhook|mqtt
			TargetURL *string  `json:"targetUrl"`
			Topic     *string  `json:"topic"`  // mqtt sink 主题
			Secret    *string  `json:"secret"` // 传空串=清除密钥
			Events    []string `json:"events"`
			Enabled   *bool    `json:"enabled"`
		}
		if err := decodeForwardsBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), nil)
			return
		}
		rule, err := fs.Update(r.PathValue("id"), forwardstore.UpdateReq{
			Name: body.Name, SinkType: body.SinkType, TargetURL: body.TargetURL, Topic: body.Topic,
			Secret: body.Secret, Events: body.Events, Enabled: body.Enabled,
		}, time.Now().UnixMilli())
		if err != nil {
			writeFwdErr(w, err)
			return
		}
		view, _ := fs.Get(rule.ID)
		writeJSON(w, http.StatusOK, view)
	})

	// ─── 删除（admin；台账随规则删除）───
	mux.HandleFunc("DELETE /api/v1/forwards/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := fs.Delete(r.PathValue("id")); err != nil {
			writeFwdErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// ─── 送达台账（新→旧）───
	mux.HandleFunc("GET /api/v1/forwards/{id}/deliveries", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := fs.Get(id); !ok {
			writeErr(w, http.StatusNotFound, "转发规则不存在", nil)
			return
		}
		limit := 0
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil {
				limit = n
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"deliveries": fs.Deliveries(id, limit)})
	})

	// ─── 测试投递（v0.59.0，spec 0032 裁决 2：同步连通性验证，绕过事件过滤）───
	mux.HandleFunc("POST /api/v1/forwards/{id}/test", func(w http.ResponseWriter, r *http.Request) {
		d, err := fs.TestDelivery(r.PathValue("id"), time.Now().UnixMilli())
		if err != nil {
			writeFwdErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"delivered": d.Status == "delivered", "attempts": d.Attempts,
			"status": d.Status, "error": d.Error, "deliveryId": d.ID,
		})
	})
}

// decodeForwardsBody 读取并解析 JSON 请求体（64KB 上限）。
func decodeForwardsBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, forwardsBodyLimit+1))
	if err != nil {
		return err
	}
	if len(body) > forwardsBodyLimit {
		return errors.New("请求体超限")
	}
	if len(body) == 0 {
		return errors.New("请求体为空")
	}
	return json.Unmarshal(body, v)
}

// writeFwdErr 归一 forwardstore 错误到状态码。
func writeFwdErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, forwardstore.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error(), nil)
	case errors.Is(err, forwardstore.ErrNotFound):
		writeErr(w, http.StatusNotFound, "转发规则不存在", nil)
	default:
		writeErr(w, http.StatusInternalServerError, err.Error(), nil)
	}
}
