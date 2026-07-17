// spike-worker — Temporal spike 的独立 worker 进程(CLAUDE.md §12 Phase 1.4)。
// 独立成二进制是为了在测试中被 SIGKILL,验证杀进程后的重放恢复。
package main

import (
	"flag"
	"log"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"hermes-devops/runtime/spike"
)

func main() {
	addr := flag.String("address", client.DefaultHostPort, "Temporal server 地址")
	flag.Parse()

	c, err := client.Dial(client.Options{HostPort: *addr})
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	w := worker.New(c, spike.TaskQueue, worker.Options{})
	w.RegisterWorkflow(spike.SpikeWorkflow)
	w.RegisterActivity(spike.FlakyDispatch)

	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker: %v", err)
	}
}
