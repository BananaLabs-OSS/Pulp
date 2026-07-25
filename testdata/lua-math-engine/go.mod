module github.com/BananaLabs-OSS/Pulp/testdata/lua-math-engine

go 1.25

require (
	github.com/BananaLabs-OSS/Fiber v0.0.0
	github.com/vmihailenco/msgpack/v5 v5.4.1
)

require github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect

replace github.com/BananaLabs-OSS/Fiber => ../../../Fiber
