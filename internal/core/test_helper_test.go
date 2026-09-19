// G1 去全局化的测试辅助：所有测试共用一份 testStd 运行时。
//
// core 包的测试没有 t.Parallel（同包串行），因此一份共享 Runtime 的语义与
// 旧的包级全局完全一致；需要隔离的测试自行 newRuntime()。原先直接给包级
// 变量赋值的地方，现在统一改写为 testStd.<字段>。
package core

// testStd 测试专用共享运行时。
var testStd = newRuntime()

// testEngine 返回绑定 testStd 的引擎（不监听端口，仅供直接调用 handler 方法）。
func testEngine() *Engine { return &Engine{rt: testStd} }
