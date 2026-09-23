//go:build !linux && !windows && !darwin

package device

import (
	"errors"
	"regexp"

	"github.com/mickeyzzc/serialtap/internal/config"
)

// Enumerate: 设备身份建立在各 OS 的专属途径上（Linux sysfs/by-path、
// Windows 注册表、macOS cu.* 命名），其余平台显式报错而非静默返回空表。
func Enumerate(exclude []*regexp.Regexp, names []config.NameRule) ([]DeviceInfo, error) {
	return nil, errors.New("serialtap 支持 Linux/Windows/macOS，当前平台不可用")
}
