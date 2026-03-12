# C2 Agent

[![Go Report Card](https://goreportcard.com/badge/github.com/garpitdreams/c2agent)](https://goreportcard.com/report/github.com/garpitdreams/c2agent)
[![Go Reference](https://pkg.go.dev/badge/github.com/garpitdreams/c2agent.svg)](https://pkg.go.dev/github.com/garpitdreams/c2agent)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**c2agent** adalah library Go (Golang) yang efisien untuk membangun agen komunikasi Command & Control (C2). Library ini mendukung protokol HTTPS dan WebSocket untuk memastikan konektivitas yang stabil dan real-time.

## ✨ Fitur Utama

* **Dual-Protocol**: Integrasi mulus antara HTTPS (REST) dan WebSockets.
* **Smart Polling**: Mekanisme interval polling yang dapat dikustomisasi.
* **Resilient**: Penanganan koneksi idle dan timeout secara otomatis.
* **Clean Exit**: Mendukung graceful shutdown untuk menjaga integritas proses.

## 🚀 Instalasi

Tambahkan library ini ke project Anda menggunakan perintah berikut:

```bash
go get https://github.com/garpitdreams/c2agent
```

## Example use
```go
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/garpitdreams/c2agent"
)

func main() {
	agent := c2agent.New(c2agent.Config{
		ServerURL:    "https://local.gamecp.id",
		ServerWS:     "wss://local.gamecp.id",
		PollInterval: 5 * time.Minute,
		IdleTimeout:  10 * time.Minute,
	})

	if err := agent.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start agent: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Agent started. Waiting for signals...")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	fmt.Println("Stopping agent...")
	agent.Stop()
}
```