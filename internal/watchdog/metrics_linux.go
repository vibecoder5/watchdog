//go:build linux

package watchdog

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Collector struct {
	total, idle uint64
	initialized bool
}

func (c *Collector) Collect(mounts []string) Snapshot {
	host, _ := os.Hostname()
	s := Snapshot{At: time.Now().UTC(), Host: host, Metrics: []Metric{}, Errors: []string{}}
	add := func(key, kind string, v float64) { s.Metrics = append(s.Metrics, Metric{key, kind, v}) }
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		s.Errors = append(s.Errors, "CPU: "+err.Error())
	} else {
		f := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
		var total, idle uint64
		for i := 1; i < len(f) && i <= 8; i++ {
			n, e := strconv.ParseUint(f[i], 10, 64)
			if e != nil {
				s.Errors = append(s.Errors, "CPU: invalid /proc/stat")
				break
			}
			total += n
			if i == 4 || i == 5 {
				idle += n
			}
		}
		if c.initialized && total > c.total && idle >= c.idle && idle-c.idle <= total-c.total {
			add("cpu", "cpu", 100*(1-float64(idle-c.idle)/float64(total-c.total)))
		}
		c.total, c.idle, c.initialized = total, idle, true
	}
	b, err = os.ReadFile("/proc/meminfo")
	if err != nil {
		s.Errors = append(s.Errors, "RAM: "+err.Error())
	} else {
		mem := map[string]float64{}
		scanner := bufio.NewScanner(strings.NewReader(string(b)))
		for scanner.Scan() {
			f := strings.Fields(scanner.Text())
			if len(f) >= 2 {
				v, e := strconv.ParseFloat(f[1], 64)
				if e == nil {
					mem[strings.TrimSuffix(f[0], ":")] = v
				}
			}
		}
		available, ok := mem["MemAvailable"]
		if mem["MemTotal"] > 0 && ok {
			add("ram", "ram", 100*(1-available/mem["MemTotal"]))
		} else {
			s.Errors = append(s.Errors, "RAM: MemAvailable/MemTotal unavailable")
		}
		if mem["SwapTotal"] > 0 {
			add("swap", "swap", 100*(1-mem["SwapFree"]/mem["SwapTotal"]))
		} else {
			add("swap", "swap", 0)
		}
	}
	for _, mount := range mounts {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(mount, &stat); err != nil {
			s.Errors = append(s.Errors, fmt.Sprintf("%s: %v", mount, err))
			continue
		}
		// Include root-reserved blocks as unavailable to ordinary services.
		if stat.Blocks > 0 {
			add("disk:"+mount, "disk", 100*(1-float64(stat.Bavail)/float64(stat.Blocks)))
		}
		if stat.Files > 0 {
			add("inode:"+mount, "inode", 100*(1-float64(stat.Ffree)/float64(stat.Files)))
		}
	}
	return s
}
