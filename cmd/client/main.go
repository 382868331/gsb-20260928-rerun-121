package main

import (
	"flag"
	"log"
	"time"

	"configdist/internal/client"
)

func main() {
	serverURL := flag.String("server", "http://127.0.0.1:8080", "服务端地址")
	id := flag.String("id", "client-1", "客户端 ID")
	data := flag.String("data", "./runtime-data/client-1", "本地数据目录")
	interval := flag.Duration("interval", 2*time.Second, "心跳间隔")
	flag.Parse()

	c, err := client.New(*id, *serverURL, *data, nil, nil)
	if err != nil {
		log.Fatalf("初始化客户端失败: %v", err)
	}
	c.Start(*interval)
	log.Printf("客户端 %s 已启动，当前实际版本 v%d", *id, c.ActiveVersion())
	select {}
}
