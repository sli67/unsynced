package main

import (
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
)

func main() {
	tmp, _ := os.Create("shibina")
	tmp.WriteString("yanks08635")
	tmp.Close()

	db, err := bolt.Open("my.db", 0o600, nil)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("data"))
		if err != nil {
			return err
		}
		return b.Put([]byte("one"), []byte("1"))
	})
	if err != nil {
		panic(err)
	}

	db.View(func(tx *bolt.Tx) error {
		fmt.Printf("one = %s\n", tx.Bucket([]byte("data")).Get([]byte("one")))
		return nil
	})
}
