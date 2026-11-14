package nodeenrollment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"io.yueyang/tom/internal/nodeapi"
)

const registrationAttempts = 3
const registrationTimeout = 10 * time.Second

// Register 在监听已建立后登记节点；已登记节点仅用签名认证，首次授权才读取一次性凭据。
// 最多三轮重试及一次认证挑战，只有传输失败、限流或服务不可用可退避重试，冲突与授权失败立即返回。
func Register(ctx context.Context, nodeID string, configuration Config, certificate tls.Certificate, readToken func(string) ([]byte, error)) error {
	if configuration.Validate() != nil || readToken == nil || len(certificate.Certificate) != 1 {
		return errors.New("节点自动登记依赖无效")
	}
	private, ok := certificate.PrivateKey.(ed25519.PrivateKey)
	if !ok {
		return errors.New("节点自动登记要求 Ed25519 证书")
	}
	fingerprints, err := nodeapi.ParseFingerprints(configuration.Server.Fingerprints)
	if err != nil {
		return err
	}
	clientTLS, err := nodeapi.ClientTLS(certificate, fingerprints)
	if err != nil {
		return err
	}
	transport := &http.Transport{TLSClientConfig: clientTLS, TLSHandshakeTimeout: registrationTimeout, ResponseHeaderTimeout: registrationTimeout, MaxIdleConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: registrationTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var token []byte
	defer func() { clear(token) }()
	for attempt := 0; attempt < registrationAttempts; attempt++ {
		claim := Claim{NodeID: nodeID, Registration: configuration.Registration, URL: configuration.Server.NodeURL, Certificate: certificate.Certificate[0], IssuedAt: time.Now().Unix()}
		encoded, err := json.Marshal(claim)
		if err != nil {
			return errors.New("节点登记内容无法编码")
		}
		body, err := json.Marshal(Request{Claim: claim, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, encoded))})
		if err != nil {
			return errors.New("节点登记签名无法编码")
		}
		status, err := sendRegistration(ctx, client, configuration.Server.URL, body, token, nodeID, configuration.Registration.ImageID)
		if status == http.StatusUnauthorized && token == nil {
			token, err = readToken(configuration.Server.TokenFile)
			if err != nil {
				return errors.New("首次接入凭据文件无法读取")
			}
			token = bytes.TrimSpace(token)
			if err := ValidateToken(token); err != nil {
				return err
			}
			status, err = sendRegistration(ctx, client, configuration.Server.URL, body, token, nodeID, configuration.Registration.ImageID)
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if status != 0 && status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable && status != http.StatusBadGateway && status != http.StatusGatewayTimeout {
			return err
		}
		if attempt == registrationAttempts-1 {
			return err
		}
		timer := time.NewTimer(time.Duration(1<<attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("节点登记未完成")
}

func sendRegistration(ctx context.Context, client *http.Client, origin string, body, token []byte, nodeID, imageID string) (status int, failure error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+Path, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("节点登记请求无法建立")
	}
	request.Header.Set("Content-Type", "application/json")
	if len(token) != 0 {
		request.Header.Set("Authorization", "Bearer "+string(token))
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, errors.New("节点登记服务连接未确认")
	}
	defer func() {
		if response.Body.Close() != nil {
			failure = errors.Join(failure, errors.New("节点登记响应关闭失败"))
		}
	}()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, errors.New("节点登记被拒绝或服务暂不可用")
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maximumBodyBytes+1))
	var receipt Receipt
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err != nil || len(encoded) > maximumBodyBytes || decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF || receipt.NodeID != nodeID || receipt.ImageID != imageID || receipt.State != "registered" {
		return http.StatusBadRequest, errors.New("节点登记回执身份或状态无效")
	}
	return http.StatusOK, nil
}

func parseToken(value string) ([]byte, error) {
	if len(value) != base64.RawURLEncoding.EncodedLen(32) {
		return nil, ErrUnauthorized
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 || strings.TrimSpace(value) != value || base64.RawURLEncoding.EncodeToString(decoded) != value {
		clear(decoded)
		return nil, ErrUnauthorized
	}
	return decoded, nil
}

// ValidateToken 校验接入码的规范编码与长度，供交互安装和节点接入共用；失败不回显输入。
func ValidateToken(value []byte) error {
	decoded, err := parseToken(string(value))
	clear(decoded)
	return err
}
