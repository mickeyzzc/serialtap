//go:build !linux && !darwin

package device

import (
	"errors"
	"regexp"

	"github.com/mickeyzzc/serialtap/internal/config"
)

// Enumerate: 仅支持 Linux 与 macOS —— 设备身份分别建立在 sysfs/by-path 与
// IOKit 注册表之上。其余平台显式报错而非静默返回空表（issue #3）。
func Enumerate(exclude []*regexp.Regexp, names []config.NameRule) ([]DeviceInfo, error) {
	return nil, errors.New("serialtap 仅支持 Linux/macOS（设备身份依赖 sysfs 或 IOKit），当前平台不可用")
}
