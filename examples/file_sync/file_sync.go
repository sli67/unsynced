package main

import "os"

func main() {
	ack, err := os.OpenFile(".ack", os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		panic(err)
	}
	f, _ := os.OpenFile("db", os.O_WRONLY|os.O_APPEND, 0700)
	f.WriteString("abc")
	ack.WriteString("abc")
	f.Sync()
	f.WriteString("shiba")
	f.Close()
}
