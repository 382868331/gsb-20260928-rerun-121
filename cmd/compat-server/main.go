// compat-server 启动多组件兼容组合发布编排的 HTTP 服务。
package main

import (
	"flag"
	"log"
	"net/http"

	"configdist/internal/compat"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8081", "监听地址")
	data := flag.String("data", "./compat-runtime", "持久化目录")
	flag.Parse()
	store, err := compat.NewStore(*data)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("compat-server 监听 %s，数据目录 %s", *addr, *data)
	log.Fatal(http.ListenAndServe(*addr, compat.NewServer(store).Handler()))
}
