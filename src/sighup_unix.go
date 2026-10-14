//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"hubproxy/config"
)

// SIGHUP 立即重载配置文件；配置文件轮询作为兜底自动生效
func init() {
	startSIGHUPReloader = func(ctx context.Context) {
		hangup := make(chan os.Signal, 1)
		signal.Notify(hangup, syscall.SIGHUP)

		go func() {
			for {
				select {
				case <-ctx.Done():
					signal.Stop(hangup)
					return
				case <-hangup:
					if err := config.ReloadConfig(); err != nil {
						fmt.Printf("SIGHUP 配置重载失败: %v\n", err)
					}
				}
			}
		}()
	}
}
