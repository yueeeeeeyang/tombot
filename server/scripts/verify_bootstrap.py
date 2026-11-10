#!/usr/bin/env python3
"""在独立验收容器验证初始化与设备生命周期；凭据只在内存中使用。"""
import json
import secrets
import subprocess
import urllib.error
import urllib.request


def verify(container_name):
    description = json.loads(subprocess.check_output(
        ['container', 'inspect', container_name], text=True, timeout=15))
    address = description[0]['status']['networks'][0]['ipv4Address'].split('/')[0]
    base = 'http://' + address + ':8787/api/v1'
    console = subprocess.check_output(
        ['container', 'exec', container_name, 'cat', '/tmp/tom-console'],
        text=True, timeout=15)
    code = console.split('：', 1)[1].strip()

    def request(method, path, payload=None, token=None, setup=False):
        headers = {'Content-Type': 'application/json'}
        if token:
            headers['Authorization'] = 'Bearer ' + token
        if setup:
            headers['X-Tom-Setup-Code'] = code
        body = None if payload is None else json.dumps(payload).encode()
        req = urllib.request.Request(base + path, body, headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=15) as response:
                content = response.read()
                return response.status, json.loads(content) if content else None
        except urllib.error.HTTPError as failure:
            return failure.code, json.loads(failure.read())

    status, bootstrap = request('GET', '/bootstrap/status')
    assert status == 200 and bootstrap['phase'] == 'ready', '真实引导未就绪'
    assert all(step['status'] in ('ok', 'degraded') for step in bootstrap['steps'])
    password = secrets.token_urlsafe(32)
    status, _ = request('POST', '/auth/setup', {'password': password, 'model': 'unsupported'}, setup=True)
    assert status == 400, '初始化未拒绝额外字段'
    status, _ = request('POST', '/auth/setup', {'password': password}, setup=True)
    assert status == 201, '真实初始化失败'
    status, session = request('POST', '/auth/login', {
        'password': password, 'device': {'name': '验收设备', 'platform': 'linux'}})
    assert status == 200, '真实登录失败'
    token = session['device_token']
    status, devices = request('GET', '/devices', token=token)
    assert status == 200 and devices[0]['is_current'], '设备状态错误'
    assert token not in json.dumps(devices), '设备列表泄漏凭据'
    status, _ = request('DELETE', '/devices/' + session['device_id'], token=token)
    assert status == 204, '设备撤销失败'
    status, _ = request('GET', '/devices', token=token)
    assert status == 401, '已撤销设备仍可访问'
    print('Ubuntu 端到端验收通过：引导、字段校验、初始化、登录、设备列表、撤销。')


if __name__ == '__main__':
    import sys
    if len(sys.argv) != 2:
        raise SystemExit('用法：python3 scripts/verify_bootstrap.py <验收容器名>')
    verify(sys.argv[1])
