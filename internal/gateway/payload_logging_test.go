package gateway

import (
	"sync"
	"testing"
)

func TestLivePayloadLoggingSettingIsConcurrentSafe(t *testing.T) {
	handler := New(nil, nil, nil, nil)
	if !handler.LogPayloads() {
		t.Fatal("package-level default payload logging changed")
	}
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func(enabled bool) {
			defer group.Done()
			for n := 0; n < 100; n++ {
				handler.SetLogPayloads(enabled)
				_ = handler.LogPayloads()
			}
		}(i%2 == 0)
	}
	group.Wait()
	handler.SetLogPayloads(false)
	if handler.LogPayloads() {
		t.Fatal("live payload logging did not report the last setting")
	}
}
