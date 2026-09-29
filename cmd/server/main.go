package main

import (
	"flag"
	"log"

	"configdist/internal/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "监听地址")
	data := flag.String("data", "data/server/state.json", "状态文件路径")
	flag.Parse()
	s, err := server.New(*data)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("配置分发服务端监听 %s，状态文件 %s", *addr, *data)
	log.Fatal(s.ListenAndServe(*addr))
}
