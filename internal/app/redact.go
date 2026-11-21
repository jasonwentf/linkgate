package app

import (
	"net/url"
	"regexp"
	"strings"
)

// urlInText 捕获错误文案里出现的 URL。覆盖面比 http(s) 更宽：
//   - 任意 scheme（ftp、socks5、redis…）；
//   - 无 scheme 的协议相对形式 //user:pass@host。
//
// 引号、反引号不作为结束符：秘密里可能就含引号（如 http://user:p'ass@host），
// 在引号处截断会把剩余秘密留在文案里。结束符只认空白与尖括号，末尾的标点
// 与引号在解析前统一剥离。
var urlInText = regexp.MustCompile(`(?i)[a-z][a-z0-9+.\-]*://[^\s<>]+|//[^\s<>]+`)

const redactedURL = "<redacted-url>"

// redactURLText 把文本里每个 URL 压成 scheme://host（协议相对形式保留 //host）。
//
// 上游 URL 可能带 user:password@ 或 ?token= 这类秘密，而 net/http、url.Parse
// 的错误文案经常原样回显整条 URL。对外返回的 4xx/5xx、事件与日志统一走这里，
// 只保留 scheme 与 host:port，去掉 userinfo、path、query 和 fragment。
// 解析失败时宁可直接整体替换成 <redacted-url>，不保留原文。
func redactURLText(s string) string {
	if s == "" || !strings.Contains(s, "/") {
		return s
	}
	return urlInText.ReplaceAllStringFunc(s, func(raw string) string {
		trimmed := strings.TrimRight(raw, ".,;:!?)]}\"'`")
		suffix := raw[len(trimmed):]
		u, err := url.Parse(trimmed)
		if err != nil || u.Host == "" {
			return redactedURL + suffix
		}
		if u.Scheme == "" {
			return "//" + u.Host + suffix
		}
		return u.Scheme + "://" + u.Host + suffix
	})
}
