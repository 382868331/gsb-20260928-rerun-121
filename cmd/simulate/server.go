package main

// 本演示不监听真实端口：服务端以 http.Handler 形式存在，
// 多个本地虚拟客户端通过进程内 RoundTripper 直接连接。
