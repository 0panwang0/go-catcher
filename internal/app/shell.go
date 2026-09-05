// 客户端外壳 HTML：iframe 内嵌 7891 监控页铺满窗口，不再放任何控制工具条——
// 服务随程序启动自动运行、退出自动停止，窗口内的状态/启停/浏览器按钮都是冗余信息。
// 服务被手动停止（托盘菜单 / /svc/stop）时，外壳轮询 vc_running 切换为居中的降级提示。
// 点窗口右上角 ✕ 由 Go 侧 WM_CLOSE 子类化拦截，自动缩到托盘（见 tray.go），无需页面按钮。
//
// 标题栏由 Windows 自己画（DWM 强制深色）；iframe 的 origin 由 src 决定，
// 嵌入 7891 的脚本仍以 7891 为 origin，跨源问题不会出现；
// 外壳自身的 JS 不发起任何 HTTP，状态判断走 Go 绑定（window.vc_running）。
package app

import "fmt"

func shellHTML() string {
	return fmt.Sprintf(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="UTF-8">
<title>VideoCatch 客户端</title>
<style>
:root{
  --bg:#0b1220;--line:#1e293b;--txt:#e2e8f0;--muted:#94a3b8;
}
*{box-sizing:border-box;margin:0;padding:0}
html,body{height:100%%;overflow:hidden}
body{
  font-family:"Segoe UI Variable","Segoe UI","Microsoft YaHei UI","Microsoft YaHei",-apple-system,Roboto,sans-serif;
  background:var(--bg);color:var(--txt);
  display:flex;flex-direction:column;
  font-feature-settings:"ss01","cv02";
  -webkit-font-smoothing:antialiased;
  -moz-osx-font-smoothing:grayscale;
  text-rendering:optimizeLegibility;
  font-smooth:always;
}
/* 隐藏自身滚动条（iframe 内的滚动条由 7891 web/index.html 自己的样式控制） */
::-webkit-scrollbar{width:0;height:0;display:none}
* { scrollbar-width:none; }

/* ===== 主体：iframe 铺满 ===== */
.main{flex:1;position:relative;background:var(--bg);overflow:hidden}
iframe{
  position:absolute;inset:0;width:100%%;height:100%%;
  border:0;background:#0b1220;
  scrollbar-width:none;
}
iframe::-webkit-scrollbar{display:none;width:0;height:0}
.placeholder{
  position:absolute;inset:0;display:flex;flex-direction:column;
  align-items:center;justify-content:center;text-align:center;padding:24px;
}
.placeholder .ico{
  width:72px;height:72px;border-radius:18px;background:#1e293b;
  display:flex;align-items:center;justify-content:center;font-size:36px;
  color:var(--muted);margin-bottom:18px;
}
.placeholder h2{font-weight:600;font-size:18px;margin-bottom:6px}
.placeholder p{color:var(--muted);font-size:13px;line-height:1.6;max-width:360px}

/* ===== 底部状态条 ===== */
.foot{
  height:26px;background:var(--bg);border-top:1px solid var(--line);
  display:flex;align-items:center;padding:0 14px;gap:14px;
  font-size:11.5px;color:var(--muted);flex-shrink:0;
}
</style></head><body>

<div class="main">
  <iframe id="frm" src=""></iframe>
  <div class="placeholder" id="ph" style="display:none">
    <div class="ico">⬇</div>
    <h2>下载服务未运行</h2>
    <p>可从托盘图标右键菜单重新启动；若启动失败，请检查端口 7891 是否被占用。</p>
  </div>
</div>

<div class="foot">
  <span>本地服务 127.0.0.1:7891 · Edge/WebView2 渲染</span>
</div>

<script>
function q(id){return document.getElementById(id)}
let iframeSrc='';
async function refresh(){
  var running=null;
  try{ running=await window.vc_running(); }catch(e){}
  if(running===null)return; // 绑定未就绪：保持现状，避免启动瞬间闪降级页
  q('ph').style.display=running?'none':'flex';
  if(running && iframeSrc!=='http://127.0.0.1:7891/'){
    q('frm').src='http://127.0.0.1:7891/';
    iframeSrc='http://127.0.0.1:7891/';
  }else if(!running && iframeSrc!==''){
    q('frm').src='';
    iframeSrc='';
  }
}
refresh();
setInterval(refresh,1500);
</script></body></html>`)
}
