// Package bobbin runs one agent step in a brand-new Podman container.
//
// A bobbin holds the thread inside a shuttle: in heedle-ai it is the
// runtime one agent step gets to itself. Every call to Runner.Run starts a
// fresh container from a pinned image, with no network, no extra
// privileges and nothing from the host mounted, and removes it afterwards.
// Whatever the step wrote inside the container (its overlay layer) is
// thrown away; only its standard output comes back.
//
// bobbin reimplements nothing: it is a typed layer over the podman command
// line, the way warp is over git. Command adapts it to draft and shuttle,
// so a program can say
//
//	(container "sha256:2301…ada4" "--claim" "the shipment will be late")
//
// and each attempt, including each Temporal retry, runs in a new container.
package bobbin
