package main

import (
	"context"

	"github.com/gorilla/websocket"
)

// execRun remains registered until its owner has finished cleaning up. Context
// cancellation works both before and after process startup, without racing with
// exec.Cmd.Start's assignment to Process.
type execRun struct {
	cancel context.CancelFunc
}

func registerExecRun(id string, cancel context.CancelFunc) (*execRun, string) {
	runningCmdsMu.Lock()
	defer runningCmdsMu.Unlock()
	if _, busy := runningCmds[id]; busy {
		return nil, "A command with this id is already running on this node."
	}
	if _, busy := runningNative[id]; busy {
		return nil, "A command with this id is already running on this node."
	}
	run := &execRun{cancel: cancel}
	runningCmds[id] = run
	return run, ""
}

func unregisterExecRun(id string, run *execRun) {
	runningCmdsMu.Lock()
	if runningCmds[id] == run {
		delete(runningCmds, id)
	}
	runningCmdsMu.Unlock()
}

// requestOutput belongs to one connection worker. Nonterminal output streams
// immediately; done is retained until all worker defers and ID cleanup finish.
// Only that worker calls send and finish, including the shell's send callback.
type requestOutput struct {
	conn     *websocket.Conn
	terminal *CommandResponse
}
type requestOutputKey struct{}

func sendRequestOutput(ctx context.Context, conn *websocket.Conn, id, typ, data string) error {
	if output, ok := ctx.Value(requestOutputKey{}).(*requestOutput); ok {
		if typ == "done" {
			if output.terminal == nil {
				output.terminal = &CommandResponse{ID: id, Type: typ, Data: data}
			}
			return nil
		}
	}
	return sendOutput(conn, id, typ, data)
}

func (output *requestOutput) finish() {
	if terminal := output.terminal; terminal != nil {
		output.terminal = nil
		sendOutput(output.conn, terminal.ID, terminal.Type, terminal.Data)
	}
}
