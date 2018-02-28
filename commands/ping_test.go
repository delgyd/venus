package commands

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPing2Nodes(t *testing.T) {
	assert := assert.New(t)

	args := []string{
		"--cmdapiaddr=:4444 --swarmlisten=/ip4/127.0.0.1/tcp/6000",
		"--cmdapiaddr=:4445 --swarmlisten=/ip4/127.0.0.1/tcp/6001",
	}

	ds := withDaemonsArgs(2, args, func() {
		id1 := getID(t, ":4444")
		id2 := getID(t, ":4445")

		t.Log("[failure] not connected")
		ping0 := run(fmt.Sprintf("go-filecoin ping --count 2 --cmdapiaddr=:4444 %s", id2))
		assert.NoError(ping0.Error)
		assert.Equal(ping0.Code, 1)
		assert.Contains(ping0.ReadStderr(), "failed to dial")
		assert.Empty(ping0.ReadStdout())

		_ = runSuccess(t, fmt.Sprintf("go-filecoin swarm connect --cmdapiaddr=:4444 /ip4/127.0.0.1/tcp/6001/ipfs/%s", id2))
		ping1 := runSuccess(t, fmt.Sprintf("go-filecoin ping --count 2 --cmdapiaddr=:4444 %s", id2))
		ping2 := runSuccess(t, fmt.Sprintf("go-filecoin ping --count 2 --cmdapiaddr=:4445 %s", id1))

		t.Log("[success] 1 -> 2")
		assert.Contains(ping1.ReadStdout(), "Pong received")

		t.Log("[success] 2 -> 1")
		assert.Contains(ping2.ReadStdout(), "Pong received")
	})
	for _, d := range ds {
		assert.NoError(d.Error)
		assert.Equal(d.Code, 0)
		assert.Empty(d.ReadStderr())
	}
}
