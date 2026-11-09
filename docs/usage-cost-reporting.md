# 用量费用（1.8.37）

- 报表字段 `usage_cost_micro_usd` 与财务扣费字段 `cost_micro_usd` 分开。
- `statistics` 记录：OAuth 使用记录写入时已经按实际上报 token、配置价格和服务档位算好的 `estimated_cost_micro_usd` 快照，不在查询时按现价重算。
- 实际服务商为 Magpie 的统计记录费用为 0，与请求中的模型别名无关。其他 API 不自动视为免费。
- 历史结算记录保留原 `cost_micro_usd`，避免改变已结算历史。Magpie 免费规则只作用于独立统计记录。
- 未匹配价格的统计记录标为 `unpriced` / 未配置价格；汇总通过 `unpriced_count` 提醒，不把缺价误称为免费。配置了零价的记录仍是已定价。
- `usage_cost_status` 可为 `priced`、`free`、`unpriced`、`settled`。
- 明细、Key/模型汇总、趋势、费用筛选统一使用用量费用。原财务字段及额度/余额/结算接口语义不变。

这只是费用报表，不产生预扣、扣款、额度消耗、结算或重定价写入；没有数据库迁移。保留 `auth_only: true`，不启用插件 Executor/ModelRouter/ResponseInterceptor，不新增请求/响应正文拷贝或解码。统计仍依赖现有 host usage 标量回调及有界 best-effort 队列：用量记录数不保证等于精确客户端请求数；丢弃/写入失败告警仍需关注。
