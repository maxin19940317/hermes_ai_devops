package adb

import (
	"context"
	"errors"
	"testing"
)

// errRunner 模拟传输层失败(adb 二进制启动失败/连接断开等),每次 Run 都返回 err。
type errRunner struct{ err error }

func (r errRunner) Run(context.Context, []string) (Result, error) { return Result{}, r.err }

// 设备在 ABI 检查通过后掉线:四次 getprop 全部因传输层失败取不到值。
// 此前的实现把每次失败静默 continue,最终返回空链——调用方把空链误读为
// "设备没有这个 SoC"(soc mismatch),真实的设备故障被伪装成配置问题。
func TestProbeChainReportsTransportFailure(t *testing.T) {
	_, err := ProbeAndroidSOCChain(context.Background(),
		errRunner{err: &LaunchError{Args: []string{"getprop"}, Err: errors.New("boom")}}, "dev1")
	if err == nil {
		t.Fatal("全部 getprop 失败时必须报错,否则空链会被上层误读为 soc mismatch")
	}
}

// okRunner 模拟调用成功但属性为空(设备在线,只是没有这个属性)。
type okRunner struct{ out string }

func (r okRunner) Run(context.Context, []string) (Result, error) {
	return Result{Stdout: r.out}, nil
}

// 属性为空但调用本身成功:设备是活的,不应报错,只是链为空。
func TestProbeChainNoErrorWhenPropsMerelyEmpty(t *testing.T) {
	chain, err := ProbeAndroidSOCChain(context.Background(), okRunner{out: ""}, "dev1")
	if err != nil {
		t.Fatalf("属性为空不是故障,不应报错: %v", err)
	}
	if len(chain) != 0 {
		t.Fatalf("chain = %v, want empty", chain)
	}
}
