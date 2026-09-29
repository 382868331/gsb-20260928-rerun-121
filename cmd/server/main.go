package main

import (
	"flag"
	"log"
	"net/http"

	"configdist/internal/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "监听地址")
	data := flag.String("data", "./runtime-data/server", "持久化目录")
	flag.Parse()

	store, err := server.NewStore(*data)
	if err != nil {
		log.Fatalf("初始化存储失败: %v", err)
	}
	srv := server.New(store)
	log.Printf("配置分发服务端监听 %s，数据目录 %s", *addr, *data)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}
