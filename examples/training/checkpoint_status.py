"""Report checkpoint progress to the GryviaAIJob's status ConfigMap and read early-checkpoint requests.

A GryviaCheckpointGuard (ai-operator --checkpoint-guard) gives each pod GRYVIA_CHECKPOINT_STATUS_CONFIGMAP and
POD_NAMESPACE, and a Role that lets the namespace's default service account get and update exactly that
ConfigMap. Rank 0 calls:

    status = checkpoint_status.from_env()
    status.committed(step)        # after a step is committed (COMMITTED moved)
    status.progress(step)         # any step; sent at most every `every_seconds`, for lost-step accounting
    if status.requested(): ...    # the guard asked for an early checkpoint (emergency trigger)

The operator copies committedStep/committedAt/currentStep into the job's status.checkpoint and uses them when it
replaces pods after a node loss. Without the environment every call is a no-op, and a failing API call is
printed and ignored: reporting must never stop training. Standard library only.
"""
from datetime import datetime, timezone
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request

SA_DIR = '/var/run/secrets/kubernetes.io/serviceaccount'


def _now():
    return datetime.now(timezone.utc).replace(microsecond=0)


def _iso(t):
    return t.strftime('%Y-%m-%dT%H:%M:%SZ')


def _parse(value):
    try:
        return datetime.strptime(value, '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=timezone.utc)
    except (TypeError, ValueError):
        return None


class StatusReporter:
    def __init__(self, configmap=None, namespace=None, api=None, token=None, context=None,
                 every_seconds=10.0, poll_seconds=10.0, clock=time.monotonic):
        self.configmap, self.namespace, self.api = configmap, namespace, api
        self.token, self.context = token, context
        self.every, self.poll, self.clock = every_seconds, poll_seconds, clock
        self._last_progress = None
        self._last_poll = None
        self._request = None

    @property
    def enabled(self):
        return bool(self.configmap and self.namespace and self.api)

    def _url(self):
        return '%s/api/v1/namespaces/%s/configmaps/%s' % (self.api, self.namespace, self.configmap)

    def _call(self, method, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self._url(), data=data, method=method)
        req.add_header('Accept', 'application/json')
        if data is not None:
            req.add_header('Content-Type', 'application/json')
        if self.token:
            req.add_header('Authorization', 'Bearer ' + self.token)
        with urllib.request.urlopen(req, timeout=10, context=self.context) as resp:
            return json.loads(resp.read())

    def _update(self, values):
        """Merge values into the ConfigMap data with an optimistic GET + PUT (the guard writes other keys)."""
        if not self.enabled:
            return False
        for _ in range(3):
            try:
                cm = self._call('GET')
                data = cm.get('data') or {}
                data.update(values)
                cm['data'] = data
                self._call('PUT', cm)
                return True
            except urllib.error.HTTPError as e:
                if e.code != 409:  # 409: someone else updated it in between; read again
                    print('checkpoint status: %s %s' % (e.code, e.reason), file=sys.stderr, flush=True)
                    return False
            except (OSError, ValueError) as e:
                print('checkpoint status: %s' % e, file=sys.stderr, flush=True)
                return False
        return False

    def committed(self, step, now=None):
        now = now or _now()
        ok = self._update({'committedStep': str(step), 'committedAt': _iso(now), 'currentStep': str(step)})
        if ok:
            self._last_progress = self.clock()
            self._request = None
        return ok

    def progress(self, step):
        t = self.clock()
        if self._last_progress is not None and t - self._last_progress < self.every:
            return False
        self._last_progress = t
        return self._update({'currentStep': str(step)})

    def requested(self):
        """The guard's open request reason (requestedAt later than committedAt), polled every poll_seconds."""
        if not self.enabled:
            return None
        t = self.clock()
        if self._last_poll is None or t - self._last_poll >= self.poll:
            self._last_poll = t
            try:
                data = self._call('GET').get('data') or {}
            except (OSError, ValueError) as e:
                print('checkpoint status: %s' % e, file=sys.stderr, flush=True)
                return self._request
            asked, done = _parse(data.get('requestedAt')), _parse(data.get('committedAt'))
            self._request = (data.get('requestReason') or 'requested') if asked and (done is None or asked >= done) else None
        return self._request


def from_env(environ=None):
    env = os.environ if environ is None else environ
    name, namespace = env.get('GRYVIA_CHECKPOINT_STATUS_CONFIGMAP'), env.get('POD_NAMESPACE')
    api = env.get('GRYVIA_KUBE_API')
    token, context = None, None
    if not api and env.get('KUBERNETES_SERVICE_HOST'):
        api = 'https://%s:%s' % (env['KUBERNETES_SERVICE_HOST'], env.get('KUBERNETES_SERVICE_PORT', '443'))
    if api and api.startswith('https://') and os.path.exists(os.path.join(SA_DIR, 'ca.crt')):
        context = ssl.create_default_context(cafile=os.path.join(SA_DIR, 'ca.crt'))
    token_path = env.get('GRYVIA_KUBE_TOKEN_FILE', os.path.join(SA_DIR, 'token'))
    if os.path.exists(token_path):
        with open(token_path) as f:
            token = f.read().strip()
    return StatusReporter(name, namespace, api, token, context)
