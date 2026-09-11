// 全局常量：嗅探列表容量与记录保留时长。
export const MAX_SNIFFED = 30;

// 嗅探记录最长保留时长：超过即视为过期，下次写入时顺手清掉（storage.local 里的
// 页面 URL / 标题 / Referer 属于隐私数据，不该无限期留存）。
export const MAX_SNIFF_AGE_MS = 7 * 24 * 60 * 60 * 1000;
