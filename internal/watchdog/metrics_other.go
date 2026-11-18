//go:build !linux

package watchdog

import (
	"os"
	"time"
)

type Collector struct{}

func (*Collector) Collect([]string) Snapshot {
	host, _ := os.Hostname()
	return Snapshot{At: time.Now().UTC(), Host: host, Metrics: []Metric{}, Errors: []string{"Сбор метрик поддерживается только на Linux; UI доступен для разработки."}}
}
