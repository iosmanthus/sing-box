package remoteusers

import (
	"testing"

	boxService "github.com/sagernet/sing-box/adapter/service"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func TestServiceRegistered(t *testing.T) {
	registry := boxService.NewRegistry()
	RegisterService(registry)
	options, loaded := registry.CreateOptions(C.TypeRemoteUsers)
	if !loaded {
		t.Fatalf("service type %q not registered", C.TypeRemoteUsers)
	}
	if _, ok := options.(*option.RemoteUsersServiceOptions); !ok {
		t.Fatalf("registered options has wrong type: %T", options)
	}
}
