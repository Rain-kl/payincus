package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Scoped read-only diagnostics; never execute guest commands or accept arbitrary file paths.
type taskLogControl struct {
	TaskID int    `json:"taskId"`
	Name   string `json:"name"`
	Action string `json:"action"`
}

var taskInstanceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,99}$`)

func streamTaskLogs(parent context.Context, out chan<- []byte, ctl taskLogControl) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Minute)
	defer cancel()
	var mu sync.Mutex
	count := 0
	send := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		if count >= 4000 {
			return
		}
		count++
		if len(line) > 7500 {
			line = line[:7500]
		}
		payload, _ := json.Marshal(map[string]any{"taskId": ctl.TaskID, "line": time.Now().UTC().Format(time.RFC3339) + " " + line})
		select {
		case out <- EncodeFrame(FrameTypeTaskLogData, 0, payload):
		case <-ctx.Done():
		}
	}
	send("节点日志采集已启动: " + ctl.Name)
	// Incus already records these files on the node. Persist the scoped copy in the panel task log.
	go followQemuLog(ctx, ctl.Name, send)
	cmd := exec.CommandContext(ctx, "journalctl", "-u", "incus", "--since", "1 minute ago", "-f", "--no-pager", "-o", "short-iso")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		send("无法读取 Incus journal: " + err.Error())
		<-ctx.Done()
		return
	}
	if err := cmd.Start(); err != nil {
		send("无法启动 Incus journal 采集: " + err.Error())
		<-ctx.Done()
		return
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	matcher := taskInstanceMatcher(ctl.Name)
	for scanner.Scan() {
		line := scanner.Text()
		if matcher.MatchString(line) {
			send("[incus] " + line)
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		send("Incus journal 读取中断: " + err.Error())
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		send("Incus journal 退出: " + err.Error() + " " + stderr.String())
	}
	if ctx.Err() == nil {
		send("Incus journal 采集结束；继续等待 QEMU 日志")
		<-ctx.Done()
	}
}

func followQemuLog(ctx context.Context, name string, send func(string)) {
	followQemuLogFile(ctx, filepath.Join("/var/log/incus", name, "qemu.log"), send)
}

func followQemuLogFile(ctx context.Context, path string, send func(string)) {
	var offset int64
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	reportedError := false
	for {
		file, err := os.Open(path)
		if err == nil {
			info, statErr := file.Stat()
			if statErr == nil {
				if info.Size() < offset {
					offset = 0
				}
				// Bound reads even if a VM generates huge logs. Do not keep file descriptors across rotation.
				if info.Size()-offset > 64*1024 {
					offset = info.Size() - 64*1024
					send("[qemu] 日志较大，仅采集最近 64 KiB")
				}
				_, _ = file.Seek(offset, io.SeekStart)
				data, readErr := io.ReadAll(io.LimitReader(file, 64*1024))
				offset += int64(len(data))
				if readErr != nil {
					send("QEMU 日志读取失败: " + readErr.Error())
				}
				for _, line := range strings.Split(string(data), "\n") {
					if line != "" {
						send("[qemu] " + line)
					}
				}
			}
			_ = file.Close()
		} else if !os.IsNotExist(err) && !reportedError {
			reportedError = true
			send(fmt.Sprintf("QEMU 日志不可读: %v", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func taskInstanceMatcher(name string) *regexp.Regexp {
	// Avoid mixing vm-1 diagnostics with vm-10.
	pattern := `(^|[^a-zA-Z0-9_.-])` + regexp.QuoteMeta(name) + `([^a-zA-Z0-9_.-]|$)`
	return regexp.MustCompile(pattern)
}

func containsTaskInstance(line, name string) bool { return taskInstanceMatcher(name).MatchString(line) }
