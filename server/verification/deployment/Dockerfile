# 专用配置验收环境，不启动正式服务、不挂载宿主控制接口。
FROM debian:trixie-slim AS isolated
RUN groupadd --gid 20001 tombot-api && useradd --uid 20001 --gid 20001 --no-create-home tombot-api
COPY deploy/node.example.json /tests/deploy/node.example.json
COPY deploy/systemd/ /tests/deploy/systemd/
WORKDIR /tests/internal/deployment
ENTRYPOINT []

FROM isolated AS forbidden
RUN groupadd docker && usermod --append --groups docker tombot-api
