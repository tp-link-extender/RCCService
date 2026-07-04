module github.com/tp-link-extender/RCCService/Proxy

go 1.26.1

require (
	github.com/TwiN/go-color v1.4.1
	github.com/joho/godotenv v1.5.1
	github.com/kovidgoyal/imaging v1.8.20
	github.com/tp-link-extender/RCCService/Shared v0.0.0-00010101000000-000000000000
)

require (
	github.com/kovidgoyal/go-parallel v1.1.1 // indirect
	github.com/kovidgoyal/go-shm v1.0.0 // indirect
	github.com/rwcarlsen/goexif v0.0.0-20190401172101-9e8deecbddbd // indirect
	golang.org/x/image v0.41.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
)

replace github.com/tp-link-extender/RCCService/Shared => ../Shared
