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
       tigervnc-standalone-server xauth x11-utils x11-xserver-utils xdotool scrot \
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
COPY install_tools.py /usr/lib/tombot/install_tools.py
COPY tools.lock.json /usr/share/tombot/tools.lock.json
RUN apt-get update \
    && python3 /usr/lib/tombot/install_tools.py \
    && dpkg-query -W -f='${Package}\t${Version}\n' > /usr/share/tombot/packages.tsv \
    && rm -rf /var/lib/apt/lists/* /root/.npm /usr/lib/tombot/install_tools.py

COPY bin/computer-agent bin/computer-task bin/computer-probe bin/bot-agent /usr/lib/tombot/
COPY desktop.py /usr/lib/tombot/desktop.py
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
