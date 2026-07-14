package handler

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// MediaHandler 媒体素材下载端点（DB 存储后端已废弃）
type MediaHandler struct {
	db *gorm.DB
	// allowedProxyHosts 是 ProxyImage 允许中转的目标域名白名单。
	// 精确匹配，或以 "." 开头表示允许该后缀的任意子域名（如 ".aliyuncs.com"）。
	allowedProxyHosts []string
}

func NewMediaHandler(db *gorm.DB) *MediaHandler {
	return &MediaHandler{db: db}
}

// WithProxyAllowedHosts 配置 ProxyImage 允许中转的目标域名白名单（通常是 OSS/CDN 域名）。
// 未配置时 ProxyImage 拒绝所有请求——不能因为漏配而默默放行任意 URL（SSRF 风险）。
func (h *MediaHandler) WithProxyAllowedHosts(hosts ...string) *MediaHandler {
	h.allowedProxyHosts = append(h.allowedProxyHosts, hosts...)
	return h
}

// ServeMedia 媒体素材已迁移至 OSS，此端点不再提供二进制数据，请通过 OSS 直链访问。
// GET /api/v1/media/:id
func (h *MediaHandler) ServeMedia(c *gin.Context) {
	respondErr(c, http.StatusNotFound, "媒体文件存储已迁移至 OSS，请通过直链访问")
}

// ProxyImage 服务端代理拉取图片，把跨域资源转成同源响应。
// GET /api/v1/media/proxy?url=<OSS 图片 URL>
//
// 用途：前端某些场景（如角色形象裁剪）需要用 canvas 读取图片像素（drawImage/toBlob），
// 若 <img> 直接指向 OSS 且未配置 CORS，浏览器的 crossorigin 请求会直接加载失败（页面显示空白）；
// 即使加载成功，canvas 也会因跨域被标记为 tainted，读取像素时抛 SecurityError。
// 经本接口中转后，前端拿到的是与自身同源的二进制数据（转成 blob: URL 使用），不再受浏览器
// 跨域限制，且不需要依赖 OSS 侧的 CORS 配置。
//
// SSRF 防护（此接口会让服务端根据用户传入的 URL 发起请求，必须严格校验，不能只判断
// http(s):// 前缀——那样等于允许客户端拿后端当任意 URL 的请求代理，可用来探测/访问内网资源、
// 云元数据接口等）：
//  1. 域名白名单：只允许 h.allowedProxyHosts 配置的域名（如 OSS/CDN 域名）。
//  2. 解析目标域名的 IP，拒绝私有/环回/链路本地/组播/未指定地址——防止域名白名单命中后，
//     实际解析到内网地址（DNS rebinding）。
//  3. 自定义 Transport.DialContext，在真正建立 TCP 连接时重新校验 IP——防止在
//     "解析校验" 和 "实际连接" 之间的时间窗口里 DNS 被重新指向内网地址（TOCTOU）。
//  4. 禁止自动跟随重定向（图片资源不应该重定向；直接跟随会绕过前三层校验）。
func (h *MediaHandler) ProxyImage(c *gin.Context) {
	rawURL := c.Query("url")
	if rawURL == "" {
		respondBadRequest(c, "url parameter required")
		return
	}

	target, err := validateProxyURL(rawURL, h.allowedProxyHosts)
	if err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		respondErr(c, http.StatusBadRequest, "invalid url")
		return
	}
	req.Header.Set("User-Agent", "InkFrame-ImageProxy/1.0")

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: safeProxyTransport(),
		// 不自动跟随重定向：重定向目标未经过上面的域名/IP 校验，直接跟随会让前面的
		// 校验形同虚设。图片资源正常情况下不需要重定向。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		reqLogger(c).Errorf("[ImageProxy] fetch %s failed: %v", rawURL, err)
		respondErr(c, http.StatusBadGateway, "failed to fetch image")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		respondErr(c, http.StatusBadGateway, "target returned a redirect, refusing to follow")
		return
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		ct = "image/jpeg"
	}
	c.Header("Cache-Control", "private, max-age=3600")
	c.DataFromReader(resp.StatusCode, resp.ContentLength, ct, resp.Body, nil)
}

// validateProxyURL 校验 rawURL 是否允许被 ProxyImage 中转：scheme 必须是 http/https，
// host 必须命中白名单，且解析出的 IP 不能是私有/环回/链路本地/组播/未指定地址。
func validateProxyURL(rawURL string, allowedHosts []string) (*url.URL, error) {
	if len(allowedHosts) == 0 {
		return nil, fmt.Errorf("image proxy not configured")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http/https URLs are allowed")
	}
	host := u.Hostname()
	if host == "" || !hostAllowed(host, allowedHosts) {
		return nil, fmt.Errorf("target host is not allowed")
	}
	if err := ensurePublicHost(host); err != nil {
		return nil, err
	}
	return u, nil
}

// hostAllowed 检查 host 是否命中白名单：完全相等，或白名单项以 "." 开头且 host 以该后缀结尾
// （如白名单 ".aliyuncs.com" 允许 "bucket.oss-cn-hangzhou.aliyuncs.com"）。
func hostAllowed(host string, allowedHosts []string) bool {
	host = strings.ToLower(host)
	for _, allowed := range allowedHosts {
		allowed = strings.ToLower(allowed)
		if strings.HasPrefix(allowed, ".") {
			if strings.HasSuffix(host, allowed) {
				return true
			}
			continue
		}
		if host == allowed {
			return true
		}
	}
	return false
}

// ensurePublicHost 解析 host 对应的所有 IP，只要有一个是私有/环回/链路本地/组播/未指定地址
// 就拒绝——即使域名本身在白名单里，也可能因 DNS rebinding 被重新指向内网。
func ensurePublicHost(host string) error {
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve host")
	}
	if len(ips) == 0 {
		return fmt.Errorf("host has no resolvable address")
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("target host resolves to a non-public address")
		}
	}
	return nil
}

func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	return true
}

// safeProxyTransport 返回一个在实际建立 TCP 连接时重新校验目标 IP 的 Transport——
// 防止在 ensurePublicHost 解析校验之后、真正连接之前的时间窗口里，DNS 被重新指向
// 内网地址（TOCTOU / DNS rebinding）。net/http 对同一个请求只会解析一次域名并直接
// 拿解析结果去连接，所以这里用 Control 钩子拦截的是 DialContext 最终要连接的具体 IP，
// 而不是重新触发一次域名解析。
func safeProxyTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			// addr 传的是域名而不是 IP（理论上此处应始终是 IP，因为 Go 的 http.Transport
			// 会先解析域名再调用 DialContext；仍做一次解析兜底）。
			resolved, lookupErr := net.LookupIP(host)
			if lookupErr != nil || len(resolved) == 0 {
				return nil, fmt.Errorf("failed to resolve host")
			}
			ip = resolved[0]
		}
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("refusing to connect to non-public address")
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	return transport
}
