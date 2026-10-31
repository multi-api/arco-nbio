module arco-nbio/bench/server

go 1.27.3

require github.com/lesismal/nbio v1.7.1

require (
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
)

replace github.com/lesismal/nbio => ../..
