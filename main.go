package main

import (
	"fmt"
	"log"
	"sync"
)

const (
	TestRoomId   = 0
	TestAkId     = ""
	TestAkSecret = ""
	TestHttpHost = "http://test-live-open.biliapi.net"
	// TestHttpHost = "https://openplatform.biliapi.com"
)

func main() {
	host, port, authBody, err := GetWebsocketInfo(TestRoomId, TestAkId, TestAkSecret)
	if err != nil {
		log.Fatalln("[main | GetWsInfo] err", err)
		return
	}
	fmt.Println(host, port, authBody)
	c := NewBiliWsClient(&BiliWsClientConfig{
		Host:     host,
		Port:     port,
		AuthBody: authBody,
		RoomId:   TestRoomId,
	})
	if c == nil {
		log.Fatalln("[main | NewBiliWsClient] client init err")
		return
	}
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		c.Run()
	}()
	wg.Wait()
	log.Println("[main | NewBiliWsClient] exit.....")
}
