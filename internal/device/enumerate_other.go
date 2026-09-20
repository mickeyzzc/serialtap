//go:build !linux

package device

import (
	"errors"
	"regexp"

	"github.com/mickeyzzc/serialtap/internal/config"
)

// Enumerate: 仅支持 Linux —— 设备身份建立在 sysfs 与 /dev/serial/by-path 之上。
// 非 Linux 平台显式报错而非静默返回空表（issue #3）。
func Enumerate(exclude []*regexp.Regexp, names []config.NameRule) ([]DeviceInfo, error) {
	return nil, errors.New("serialtap 仅支持 Linux（sysfs/by-path 设备身份），当前平台不可用")
}
