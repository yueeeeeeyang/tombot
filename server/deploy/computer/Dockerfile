FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

LABEL org.opencontainers.image.title="Bot 远程电脑" \
      org.opencontainers.image.description="Debian 13、XFCE、xfwm4、X11，1280×800 独立桌面"

ENV PATH=/usr/local/go/bin:/usr/local/node/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PYTHONDONTWRITEBYTECODE=1 GOTOOLCHAIN=local

# 不启动宿主服务；图形会话由电脑代理按 Bot 身份启动，不运行显示登录管理器。
RUN printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d \
    && chmod 0755 /usr/sbin/policy-rc.d \
    && printf 'Acquire::Retries "3";\nAcquire::http::Timeout "30";\nAcquire::https::Timeout "30";\nAcquire::http::Pipeline-Depth "0";\n' > /etc/apt/apt.conf.d/80tombot-download \
    && apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates \
    && sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources \
    && rm -rf /var/lib/apt/lists/*

RUN printf 'path-include /usr/share/locale/zh*\n' > /etc/dpkg/dpkg.cfg.d/zz-tombot-locales \
    && apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 install -y --no-install-recommends \
       locales dbus dbus-x11 libnss-wrapper at-spi2-core \
       xfce4 xfce4-terminal xfwm4 thunar mousepad \
       tigervnc-standalone-server xvfb xauth x11-utils x11-xserver-utils xdotool scrot python3-xlib python3-pil \
       fonts-noto-cjk fonts-noto-color-emoji fonts-dejavu-core \
       bash bash-completion coreutils procps util-linux \
       python3 python3-pip python3-venv python-is-python3 rustc cargo perl build-essential pkg-config \
       git gh curl wget jq ripgrep tmux openssh-client openssl \
       ffmpeg poppler-utils postgresql-client \
       libreoffice-writer libreoffice-calc libreoffice-impress libreoffice-draw libreoffice-gtk3 libreoffice-l10n-zh-cn \
       unzip zip xz-utils \
    && printf 'zh_CN.UTF-8 UTF-8\nen_US.UTF-8 UTF-8\n' > /etc/locale.gen \
    && locale-gen \
    && ln -s "$(dpkg -L libnss-wrapper | sed -n '/\/libnss_wrapper\.so$/p')" /usr/lib/tombot-nss-wrapper.so \
    && install -d -m 0755 /usr/lib/tombot /usr/share/tombot /workspace \
    && rm -rf /var/lib/apt/lists/*

ENV LANG=zh_CN.UTF-8 LC_ALL=zh_CN.UTF-8
# 原生窗口隔离扩展使用已验证的 Xpra 协议版本；更新必须同时运行窗口越权和双桌面回归。
COPY tools.lock.json /usr/share/tombot/tools.lock.json
RUN curl --fail --location --retry 3 --connect-timeout 15 --max-time 60 https://xpra.org/xpra.asc -o /usr/share/keyrings/xpra.asc \
    && printf '4df3209ea4b759f0b5838b60baedb6893ec62df7abad9046462176a6e6a7ed7d  /usr/share/keyrings/xpra.asc\n' | sha256sum -c - \
    && printf 'Types: deb\nURIs: https://xpra.org\nSuites: trixie\nComponents: main\nSigned-By: /usr/share/keyrings/xpra.asc\n' > /etc/apt/sources.list.d/xpra.sources \
    && apt-get update \
    && xpra_version="$(jq -er --arg arch "$(dpkg --print-architecture)" '.xpra[$arch]' /usr/share/tombot/tools.lock.json)" \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "xpra=$xpra_version" "xpra-x11=$xpra_version" \
    && rm -rf /var/lib/apt/lists/*
COPY install_tools.py /usr/lib/tombot/install_tools.py
RUN apt-get update \
    && python3 /usr/lib/tombot/install_tools.py \
    && dpkg-query -W -f='${Package}\t${Version}\n' > /usr/share/tombot/packages.tsv \
    && rm -rf /var/lib/apt/lists/* /root/.npm /usr/lib/tombot/install_tools.py

COPY bin/computer-agent bin/computer-task bin/computer-probe bin/bot-agent /usr/lib/tombot/
COPY desktop.py /usr/lib/tombot/desktop.py
COPY files.py /usr/lib/tombot/files.py
COPY native_browser.py native_browser_io.py native_browser_policy.py /usr/lib/tombot/
COPY prepare.py /usr/lib/tombot/prepare.py
COPY desktop-environment.json /usr/share/tombot/desktop-environment.json
COPY verify_tools.py /usr/lib/tombot/verify_tools.py
COPY seccomp.json seccomp-source.json seccomp.LICENSE /usr/share/tombot/
COPY xfce4-session.xml /etc/xdg/xfce4/xfconf/xfce-perchannel-xml/xfce4-session.xml
COPY xfwm4.xml /etc/xdg/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml
COPY xfce4-desktop.xml /etc/xdg/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml
RUN chmod 0755 /usr/lib/tombot/computer-agent /usr/lib/tombot/computer-task /usr/lib/tombot/computer-probe /usr/lib/tombot/bot-agent \
    && install -d -m 0755 /var/lib/tombot-browser \
    && chmod 0644 /usr/lib/tombot/desktop.py \
    && test "$(. /etc/os-release; printf '%s' "$VERSION_CODENAME")" = trixie \
    && test -x /usr/bin/Xtigervnc \
    && test -x /usr/bin/xfwm4 \
    && test -f /usr/share/locale/zh_CN/LC_MESSAGES/xfce4-session.mo \
    && python3 /usr/lib/tombot/verify_tools.py > /usr/share/tombot/tools-versions.json

LABEL io.tombot.computer.storage-layout="2"
WORKDIR /workspace
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/lib/tombot/computer-agent"]
