// sample module
module example.com/app

go 1.22

toolchain go1.22.3

require (
	github.com/gorilla/mux v1.8.0
	golang.org/x/text v0.3.7 // indirect
	gopkg.in/yaml.v3 v3.0.1
	github.com/old/thing v2.0.0+incompatible // indirect
	example.com/local v1.0.0
	example.com/pseudo v0.0.0-20191109021931-daa7c04131f5 // indirect
)

require github.com/pkg/errors v0.9.1

replace example.com/local => ../local

replace github.com/pkg/errors v0.9.1 => github.com/pkg/errors v0.9.2

exclude golang.org/x/net v0.0.1

retract [v1.0.0, v1.0.5]
