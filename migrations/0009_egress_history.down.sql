-- 回滚：历史是派生数据（当前绑定在 egress_bindings 里不受影响），可直接丢。
DROP TABLE IF EXISTS egress_binding_history;
