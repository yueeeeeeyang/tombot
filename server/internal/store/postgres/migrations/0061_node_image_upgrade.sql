-- 已绑定节点仅可更新镜像身份；地址、证书与节点主键仍不可改绑。
GRANT UPDATE (image_id) ON tombot.node_connections TO tombot_control;
