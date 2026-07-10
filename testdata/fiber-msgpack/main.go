// A legacy-style Fiber sibling cell: it registers ONLY a msgpack Provider and
// never calls ProvideRaw, so it does NOT export pulp_post_return. Used to prove
// (1) the msgpack sibling path is byte-for-byte unchanged after the additive
// witcell graft, and (2) the host gate routes a no-post_return cell to the
// opaque Call path (CallTyped refuses it with ErrNoPostReturn).
package main

import "github.com/BananaLabs-OSS/Fiber/pulp"

func main() {}

func init() {
	pulp.Provide("echo.reverse", func(args []byte) ([]byte, error) {
		out := make([]byte, len(args))
		for i := range args {
			out[i] = args[len(args)-1-i]
		}
		return out, nil
	})
}
