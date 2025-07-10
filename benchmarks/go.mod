module benchmark

go 1.21

require (
	github.com/allegro/bigcache/v3 v3.1.0
	github.com/coocood/freecache v1.2.4
	github.com/dgraph-io/ristretto v0.1.1
	github.com/patrickmn/go-cache v2.1.0+incompatible
	github.com/unkn0wn-root/kioshun v0.0.3
)

require (
	github.com/cespare/xxhash/v2 v2.1.2 // indirect
	github.com/dustin/go-humanize v1.0.0 // indirect
	github.com/golang/glog v0.0.0-20160126235308-23def4e6c14b // indirect
	github.com/pkg/errors v0.9.1 // indirect
	golang.org/x/sys v0.0.0-20221010170243-090e33056c14 // indirect
)

replace github.com/unkn0wn-root/kioshun => ../
