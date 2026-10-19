package main

import "os"

func main() {
	f, _ := os.OpenFile("db", os.O_WRONLY|os.O_CREATE, 0700)
	f.WriteString("abc")
	f.Sync()
	f.WriteString("shiba")
	f.Close()
}
