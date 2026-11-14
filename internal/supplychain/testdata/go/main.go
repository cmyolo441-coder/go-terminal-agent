package main

import (
	"fmt"

	"github.com/gorilla/mux"
	yaml "gopkg.in/yaml.v3"
)

func main() {
	r := mux.NewRouter()
	var v any
	_ = yaml.Unmarshal(nil, &v)
	fmt.Println(r)
}
