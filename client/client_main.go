package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/akashrampure/wslib/utils"
)

func main() {
	logger := log.New(os.Stdout, "[ws-client] ", log.LstdFlags|log.Llongfile)

	config := NewClientConfig("ws", "localhost", "8080", "/ws", 3, nil)

	callbacks := &ClientCallbacks{
		Started: func() {
			fmt.Println("Client started")
		},
		Stopped: func() {
			fmt.Println("Client stopped")
		},
		OnConnect: func() {
			fmt.Println("Connected to server")
		},
		OnDisconnect: func(err error) {
			fmt.Println("Disconnected from server:", err)
		},
		OnMessage: func(msg []byte) {
			fmt.Println("Received message:", string(msg))
		},
		OnError: func(err error) {
			fmt.Println("Error:", err)
		},
	}

	client := NewClient(config, callbacks, logger)

	client.Start()

	go func() {
		time.Sleep(5 * time.Second)
		err := client.Send(map[string]string{"type": "ping", "message": "Hello from client"})
		if err != nil {
			logger.Println("Send failed:", err)
		}
	}()

	utils.CloseSignal()

	logger.Println("Shutting down client...")
	client.Stop()
}
