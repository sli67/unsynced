package main

import "os"

func main() {
	os.Mkdir("db_dir", 0777)
	f, _ := os.Create("db_dir/db")
	f2, _ := os.Create("db_dir/db.new")
	f.WriteString("oldstring")
	f2.WriteString("newString")
	f.Sync()
	f2.Sync()
	f.Close()
	f2.Close()

	dir, _ := os.Open("db_dir")
	dir.Sync()
	dir.Close()

	os.Rename("db_dir/db.new", "db_dir/db")
}
