package nodeenrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapi"
)

const BootstrapPath = "/api/v2/node-bootstrap"
const MaximumJoinCode = 2048
const joinPrefix = "tomnode1."

// JoinCode 通过已登录管理平台交付首次信任；不得写入参数、日志或命令行。
type JoinCode struct {
	URL          string   `json:"url"`
	Fingerprints []string `json:"fingerprints"`
	Token        string   `json:"token"`
}

// EncodeJoinCode 将服务来源、证书固定值和一次性凭据封装为可粘贴接入码。
func EncodeJoinCode(value JoinCode) (string, error) {
	if nodeapi.ValidateURL(value.URL) != nil || len(value.URL) > 512 || ValidateToken([]byte(value.Token)) != nil {
		return "", ErrUnauthorized
	}
	if _, err := nodeapi.ParseFingerprints(value.Fingerprints); err != nil {
		return "", ErrUnauthorized
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", ErrUnauthorized
	}
	defer clear(encoded)
	code := joinPrefix + base64.RawURLEncoding.EncodeToString(encoded)
	if len(code) > MaximumJoinCode {
		return "", ErrUnauthorized
	}
	return code, nil
}

// ParseJoinCode 只接受规范封装，拒绝重复字段、未知字段和旧裸凭据，不回显输入。
func ParseJoinCode(encoded []byte) (JoinCode, error) {
	var value JoinCode
	if len(encoded) > MaximumJoinCode || !bytes.HasPrefix(encoded, []byte(joinPrefix)) {
		return value, errors.New("接入码格式无效，请从新版管理平台重新生成")
	}
	body, err := base64.RawURLEncoding.DecodeString(string(encoded[len(joinPrefix):]))
	if err != nil {
		return value, ErrUnauthorized
	}
	defer clear(body)
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return JoinCode{}, ErrUnauthorized
	}
	canonical, err := EncodeJoinCode(value)
	if err != nil || canonical != string(encoded) {
		return JoinCode{}, ErrUnauthorized
	}
	return value, nil
}

type BootstrapConfiguration struct {
	ClientFingerprints    []string           `json:"client_fingerprints"`
	VerificationResources computer.Resources `json:"verification_resources"`
}

func (configuration BootstrapConfiguration) Validate() error {
	if _, err := nodeapi.ParseFingerprints(configuration.ClientFingerprints); err != nil {
		return err
	}
	return configuration.VerificationResources.Validate()
}

// Bootstrap 将签发与只读配置下发组合，节点实际登记仍是唯一消费凭据的事务。
type Bootstrap struct {
	service       *Service
	location      JoinCode
	configuration BootstrapConfiguration
	slots         chan struct{}
	logger        *slog.Logger
}

func NewBootstrap(service *Service, origin string, fingerprints []string, configuration BootstrapConfiguration, logger *slog.Logger) (*Bootstrap, error) {
	if logger == nil || service == nil || nodeapi.ValidateURL(origin) != nil || len(origin) > 512 || configuration.Validate() != nil {
		return nil, errors.New("节点引导配置无效")
	}
	if _, err := nodeapi.ParseFingerprints(fingerprints); err != nil {
		return nil, err
	}
	return &Bootstrap{service: service, location: JoinCode{URL: origin, Fingerprints: append([]string(nil), fingerprints...)}, configuration: configuration, slots: make(chan struct{}, 16), logger: logger}, nil
}
func (bootstrap *Bootstrap) Issue(ctx context.Context, actor identity.Principal, requestID string) (Grant, error) {
	grant, err := bootstrap.service.Issue(ctx, actor, requestID)
	if err != nil {
		return Grant{}, err
	}
	code := bootstrap.location
	code.Token = grant.Token
	grant.Token, err = EncodeJoinCode(code)
	if err != nil {
		return Grant{}, err
	}
	return grant, nil
}

func (service *Service) authorizeBootstrap(ctx context.Context, token string) error {
	raw, err := parseToken(token)
	if err != nil {
		return ErrUnauthorized
	}
	defer clear(raw)
	digest := sha256.Sum256(raw)
	err = service.control.Platform(ctx, func(tx *sql.Tx) error {
		var found bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.node_enrollment_tokens WHERE token_hash=$1 AND consumed_node_id IS NULL AND expires_at>clock_timestamp())`, digest[:]).Scan(&found); err != nil {
			return err
		}
		if !found {
			return ErrUnauthorized
		}
		return nil
	})
	return transactionError(err)
}

// ServeHTTP 仅向未过期、未消费的接入凭据提供公开配置，拒绝浏览器来源及无界请求。
func (bootstrap *Bootstrap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet || r.URL.Path != BootstrapPath || r.URL.RawQuery != "" || r.Header.Get("Origin") != "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	select {
	case bootstrap.slots <- struct{}{}:
		defer func() { <-bootstrap.slots }()
	default:
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	authorization := r.Header.Get("Authorization")
	if len(authorization) != 50 || !strings.HasPrefix(authorization, "Bearer ") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := bootstrap.service.authorizeBootstrap(ctx, authorization[7:]); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			w.WriteHeader(http.StatusUnauthorized)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	// 配置在构造时已校验且仅含公开值，先编码完成再写响应。
	encoded, err := json.Marshal(bootstrap.configuration)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(encoded); err != nil {
		bootstrap.logger.WarnContext(ctx, "节点引导响应发送未完成", "request_id", ids.New())
	}
}

// FetchBootstrap 先核对接入码固定的证书，再发送凭据；仅发起一次有界只读请求，不跟随重定向。
func FetchBootstrap(ctx context.Context, code JoinCode) (configuration BootstrapConfiguration, result error) {
	if _, err := EncodeJoinCode(code); err != nil {
		return configuration, err
	}
	pins, err := nodeapi.ParseFingerprints(code.Fingerprints)
	if err != nil {
		return configuration, err
	}
	trust, err := nodeapi.PinnedTLS(pins)
	if err != nil {
		return configuration, err
	}
	transport := &http.Transport{TLSClientConfig: trust, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxIdleConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, code.URL+BootstrapPath, nil)
	if err != nil {
		return configuration, errors.New("节点引导请求无法创建")
	}
	request.Header.Set("Authorization", "Bearer "+code.Token)
	response, err := client.Do(request)
	if err != nil {
		return configuration, errors.New("无法连接服务端或证书不匹配，请检查接入码及服务地址")
	}
	defer func() {
		if response.Body.Close() != nil {
			result = errors.Join(result, errors.New("关闭节点引导响应失败"))
		}
	}()
	if response.StatusCode != http.StatusOK {
		return configuration, errors.New("服务端拒绝接入码，请检查有效期或重新生成")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumBodyBytes+1))
	if err != nil || len(body) > maximumBodyBytes {
		return configuration, errors.New("节点引导响应超限或不完整")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&configuration) != nil || decoder.Decode(new(any)) != io.EOF || configuration.Validate() != nil {
		return BootstrapConfiguration{}, errors.New("服务端节点配置无效")
	}
	return configuration, nil
}
