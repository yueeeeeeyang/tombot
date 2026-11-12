# 仅为节点入口集成测试补充宿主工具，不发布为用户电脑镜像。
ARG TOM_TEST_BASE_IMAGE=debian:trixie-slim
FROM ${TOM_TEST_BASE_IMAGE}
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends e2fsprogs util-linux mount \
    && rm -rf /var/lib/apt/lists/*
