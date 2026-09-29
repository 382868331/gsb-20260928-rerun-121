package main

import (
	"flag"
	"log"

	"configdist/internal/client"
)

func main() {
	id := flag.String("id", "client-1", "客户端 ID")
	addr := flag.String("addr", "127.0.0.1:19001", "本地监听地址")
	serverURL := flag.String("server", "http://127.0.0.1:18080", "服务端地址")
	dataDir := flag.String("data", "data/client1", "状态目录")
	flag.Parse()
	c, err := client.New(*id, *serverURL, *dataDir)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("客户端 %s 监听 %s", *id, *addr)
	log.Fatal(c.Serve(*addr))
}
