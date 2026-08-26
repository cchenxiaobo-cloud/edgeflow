// 发布前镜像存在性探活（v0.9.0，R-1）。
//
// 背景：模型发布创建时对目标版本 mirror 做 registry 存在性检查——发布
// "成功"（podsync+config-sync acked）≠ 镜像可用（拉取在边缘，PodStatus
// 暴露，KNOWN-ISSUES 语义）。R-1 提供发布前探活：配置为 warn 时镜像
// 不存在/registry 不可达仅告警（发布照常）；fail 时阻断（422，业务前置
// 不满足）。默认 off（零行为变化）。
//
// 实现：Docker Registry HTTP API v2：
//   - 私有 registry：HEAD /v2/<repo>/manifests/<tag>（可带 Bearer token）；
//   - Docker Hub（repo 无 '/' 或 registry 推断为 docker.io）：先
//     GET /v2/ 拿 WWW-Authenticate（Bearer realm/service）→ 换 token →
//     HEAD manifests；
//   - Accept 头带 Docker 分发 manifest 类型（部分 registry 对缺 Accept
//     的 HEAD 返回 406）。
package modelrelease

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MirrorCheckMode 是镜像探活模式。
type MirrorCheckMode int

const (
	// MirrorCheckOff 默认：不检查（零行为变化）。
	MirrorCheckOff MirrorCheckMode = iota
	// MirrorCheckWarn 探活失败仅告警（发布照常创建）。
	MirrorCheckWarn
	// MirrorCheckFail 探活失败阻断发布（422）。
	MirrorCheckFail
)

// ParseMirrorCheckMode 解析 env 值：空/off=Off；warn=Warn；fail=Fail；其余报错。
func ParseMirrorCheckMode(v string) (MirrorCheckMode, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "0", "false":
		return MirrorCheckOff, nil
	case "warn", "1", "true":
		return MirrorCheckWarn, nil
	case "fail":
		return MirrorCheckFail, nil
	}
	return MirrorCheckOff, fmt.Errorf("modelrelease: 非法镜像探活模式 %q（支持 off/warn/fail）", v)
}

// MirrorCheckOptions 是探活配置（装配层从 env 解析注入）。
type MirrorCheckOptions struct {
	Mode    MirrorCheckMode
	Timeout time.Duration // 单次探活超时（默认 5s）
	Token   string        // registry Bearer token（私有 registry 用；空=不携带）
	// HubEndpoint 是 Docker Hub 探活端点（测试注入用；空 = 默认
	// https://registry-1.docker.io）。
	HubEndpoint string
	// HTTPClient 可注入（测试用）；nil → http.DefaultClient。
	HTTPClient *http.Client
}

// DefaultMirrorCheckTimeout 是探活超时默认值（env 缺省）。
const DefaultMirrorCheckTimeout = 5 * time.Second

// mirrorParts 是解析后的镜像三段（registry/repo/tag）。
type mirrorParts struct {
	Registry string // 空 = Docker Hub
	Repo     string
	Tag      string
}

// parseMirror 拆分 mirror 字符串：<[scheme://][registry/]>repo[:tag]。
//   - tag 缺省 → "latest"；
//   - 无 '/' 或 registry 为 docker.io/registry-1.docker.io → Docker Hub；
//   - registry 可带显式 scheme（http:// 内网 registry；缺省 https）。
func parseMirror(mirror string) (mirrorParts, error) {
	mirror = strings.TrimSpace(mirror)
	if mirror == "" {
		return mirrorParts{}, fmt.Errorf("modelrelease: mirror 为空")
	}
	// 拆分 tag（最后一个 ':' 之后；排除 registry 端口形态——':' 在 '/' 前）
	repoPart, tag := mirror, "latest"
	if i := strings.LastIndex(mirror, ":"); i >= 0 {
		slash := strings.Index(mirror, "/")
		if i > slash {
			repoPart, tag = mirror[:i], mirror[i+1:]
		}
	}
	if tag == "" {
		tag = "latest"
	}
	registry, repo := "", repoPart
	if strings.Contains(repoPart, "/") {
		// 先剥 scheme（http:// https://），避免 "http:" 被误判为 registry
		scheme := ""
		rest := repoPart
		if j := strings.Index(rest, "://"); j >= 0 {
			scheme, rest = rest[:j+3], rest[j+3:]
		}
		first := rest[:strings.Index(rest, "/")]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			registry = scheme + first
			repo = rest[len(first)+1:]
		} else if scheme != "" {
			// 显式 scheme 但首个段不是 registry host（如 http://repo/tag）→ 按 Docker Hub 语义
			repo = rest
		}
	}
	if strings.HasPrefix(repo, "/") || repo == "" {
		return mirrorParts{}, fmt.Errorf("modelrelease: mirror %q 解析失败（repo 为空）", mirror)
	}
	return mirrorParts{Registry: registry, Repo: repo, Tag: tag}, nil
}

// manifestAccept 是 Docker 分发 manifest 的 Accept 头（部分 registry 校验）。
const manifestAccept = "application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/json"

// CheckMirror 检查镜像在 registry 中是否存在。返回 nil = 存在；错误 =
// 不存在或 registry 不可达（调用方按模式决定 warn/fail）。
func CheckMirror(ctx context.Context, mirror string, opts MirrorCheckOptions) error {
	parts, err := parseMirror(mirror)
	if err != nil {
		return err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultMirrorCheckTimeout
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := "https://registry-1.docker.io"
	if parts.Registry != "" {
		// registry 段可带显式 scheme（http:// 内网 registry）；缺省 https
		if strings.Contains(parts.Registry, "://") {
			endpoint = parts.Registry
		} else {
			endpoint = "https://" + parts.Registry
		}
	} else if opts.HubEndpoint != "" {
		endpoint = opts.HubEndpoint // 测试注入
	}
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", endpoint, parts.Repo, parts.Tag)

	// 私有 registry：直接 HEAD；Docker Hub：先换 Bearer token。
	if parts.Registry == "" {
		token, terr := dockerHubToken(ctx, client, parts.Repo, endpoint)
		if terr != nil {
			return fmt.Errorf("modelrelease: Docker Hub token 获取失败: %w", terr)
		}
		opts.Token = token
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
	if err != nil {
		return fmt.Errorf("modelrelease: 构造探活请求失败: %w", err)
	}
	req.Header.Set("Accept", manifestAccept)
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("modelrelease: 镜像探活请求失败（registry 不可达?）: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("modelrelease: 镜像 %s 在 registry 中不存在（404）", mirror)
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("modelrelease: 镜像探活被拒（401）——registry 需要鉴权，请配置 EDGEFLOW_CLOUDCORE_REGISTRY_TOKEN（私有 registry）或检查网络策略（Docker Hub）")
	default:
		return fmt.Errorf("modelrelease: 镜像探活异常状态码 %d（mirror=%s）", resp.StatusCode, mirror)
	}
}

// dockerHubToken 执行 Docker Hub 的 token 换取流程：
// GET <hub>/v2/ → 401 + WWW-Authenticate: Bearer realm=...,service=... →
// GET realm?service=...&scope=repository:<repo>:pull → {"token": ...}。
// hubEndpoint 为测试注入点（默认 registry-1.docker.io）。
func dockerHubToken(ctx context.Context, client *http.Client, repo, hubEndpoint string) (string, error) {
	if hubEndpoint == "" {
		hubEndpoint = "https://registry-1.docker.io"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubEndpoint+"/v2/", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusUnauthorized {
		return "", fmt.Errorf("期望 401 拿 WWW-Authenticate，实际 %d", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	params := parseAuthChallenge(challenge)
	realm, service := params["realm"], params["service"]
	if realm == "" {
		return "", fmt.Errorf("WWW-Authenticate 缺 realm: %q", challenge)
	}
	tokenURL := realm + "?service=" + service + "&scope=repository:" + repo + ":pull"
	treq, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	tresp, err := client.Do(treq)
	if err != nil {
		return "", err
	}
	defer func() { _ = tresp.Body.Close() }()
	if tresp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token 端点返回 %d", tresp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(tresp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("token 响应解析失败: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("token 响应无 token 字段")
}

// parseAuthChallenge 解析 WWW-Authenticate: Bearer realm="...",service="..."
// 为键值 map（宽松解析：引号可选）。
func parseAuthChallenge(h string) map[string]string {
	out := make(map[string]string)
	h = strings.TrimSpace(h)
	if i := strings.Index(h, " "); i >= 0 {
		h = h[i+1:]
	}
	for _, kv := range strings.Split(h, ",") {
		kv = strings.TrimSpace(kv)
		eq := strings.Index(kv, "=")
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(kv[:eq])
		v := strings.TrimSpace(kv[eq+1:])
		v = strings.Trim(v, `"`)
		out[k] = v
	}
	return out
}
