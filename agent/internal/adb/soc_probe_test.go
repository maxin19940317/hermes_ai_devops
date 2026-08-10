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

// mixedRunner 按 getprop 的属性名(args 最后一个元素)分派:命中 failProps 的
// 属性返回传输层失败,其余返回 soc(合法 SoC 值)。用于构造"链上部分调用
// 失败、部分调用探到有效值"的混合场景——前两条用例(errRunner 恒失败 /
// okRunner 恒成功但值恒为空)分别只覆盖了 out 恒空的两端,都不会拦住把
// 返回条件从 `len(out) == 0 && lastErr != nil` 误简化为 `lastErr != nil`
// 的退化(2026-08-09 评审 Important)。
type mixedRunner struct {
	failProps map[string]bool
	soc       string
}

func (r mixedRunner) Run(_ context.Context, args []string) (Result, error) {
	prop := args[len(args)-1]
	if r.failProps[prop] {
		return Result{}, &LaunchError{Args: args, Err: errors.New("boom")}
	}
	return Result{Stdout: r.soc}, nil
}

// 链上前两跳(ro.soc.model / ro.chipname)传输层失败,后两跳
// (ro.board.platform / ro.product.board)调用成功并探到合法值:
// 设备显然是活的,即便过程中出现过失败也不应报错,且失败不能吞掉后面
// 探到的有效值。
func TestProbeChainNoErrorWhenSomePropsFailButOthersSucceed(t *testing.T) {
	runner := mixedRunner{
		failProps: map[string]bool{"ro.soc.model": true, "ro.chipname": true},
		soc:       "trinket",
	}
	chain, err := ProbeAndroidSOCChain(context.Background(), runner, "dev1")
	if err != nil {
		t.Fatalf("链上探到了有效值,即便部分调用失败也不应报错: %v", err)
	}
	if len(chain) != 1 || chain[0] != "trinket" {
		t.Fatalf("chain = %v, want [trinket](失败的属性不应吞掉后面探到的有效值)", chain)
	}
}
