"""Read-only observation of an existing parser job and BlankTrail log tail.

No jobs, ports, settings, captures or logging levels are changed. The diagnostic
preview endpoint collects an in-memory, redacted log tail; it sends no report.
Credentials are read only to authenticate to the configured control API.
Only allowlisted metadata and upstream hashes are written to the output.
"""
import argparse
import datetime as dt
import hashlib
import json
import re
import sqlite3
import time
import urllib.request


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--settings', default='C:/Sandbox/gserp-settings.json')
    ap.add_argument('--database', default='C:/Sandbox/gserp.db')
    ap.add_argument('--job', type=int, required=True)
    ap.add_argument('--output', required=True)
    ap.add_argument('--samples', type=int, default=13)
    ap.add_argument('--interval', type=float, default=30)
    args = ap.parse_args()
    settings = json.load(open(args.settings, encoding='utf-8-sig'))
    # Direct LAN access: system HTTP proxy settings can time out this address.
    op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    base = settings['control_url'].rstrip('/')
    known_ports = set()

    def fetch(path, preview=False, parser=False):
        req = urllib.request.Request(
            ('http://127.0.0.1:8080' if parser else base) + path,
            headers={} if parser else {'X-API-Key': settings['api_key']},
            data=b'{}' if preview else None,
            method='POST' if preview else 'GET')
        with op.open(req, timeout=12) as response:
            return response.read().decode('utf-8')

    def digest(value):
        return hashlib.sha256(value.encode()).hexdigest()[:20]

    def safe_error(value):
        # Store categories only, never connection strings or response contents.
        text = value.lower()
        return [label for label in ['timeout', 'deadline exceeded', 'eof',
                'refused', 'reset', 'tls', 'certificate', 'cancel', 'socks',
                'resolve', 'no route', 'network is unreachable'] if label in text]

    event_keys = {'ts', 'level', 'msg', 'port', 'host', 'status', 'family',
                  'duration_ms', 'elapsed_ms', 'attempt', 'count', 'changed',
                  'got_headers', 'profile', 'stream', 'reason', 'vendor'}
    port_keys = {'port', 'created_at', 'last_activity', 'current_profile',
                 'max_concurrent', 'keep_sessions', 'js_solver', 'skip_retry',
                 'captcha_action', 'effective_idle_seconds', 'vdns_mode'}
    seen_events = set()
    with open(args.output, 'x', encoding='utf-8', buffering=1) as output:
        def emit(data):
            output.write(json.dumps(data, ensure_ascii=False) + '\n')

        emit({'event': 'start', 'job': args.job, 'samples': args.samples,
              'interval_seconds': args.interval, 'read_only': True,
              'solver_counters_scope': 'whole_service',
              'ui_counts_scope': 'currently active parser pool; check progress.running',
              'port_selection': 'keep_sessions=true, port in [20000,21000)'})
        for index in range(args.samples):
            began = time.monotonic()
            snapshot = {'event': 'snapshot', 'index': index,
                        'at': dt.datetime.now(dt.timezone.utc).isoformat()}
            errors = []
            for name, path in [('status', '/api/v1/status'),
                               ('ports', '/api/v1/ports'),
                               ('solver', '/api/v1/solver/stats'),
                               ('queue', '/api/v1/solver/queue'),
                               ('preview', '/api/v1/bug-report/preview')]:
                try:
                    value = json.loads(fetch(path, preview=name == 'preview'))
                    if name == 'status':
                        snapshot[name] = {k: value.get(k) for k in
                            ['version', 'commit', 'uptime', 'open_ports',
                             'max_ports', 'served_connections']}
                    elif name == 'ports':
                        rows = [p for p in value['ports'] if
                                20000 <= p['port'] < 21000 and p['keep_sessions']]
                        known_ports.update(p['port'] for p in rows)
                        snapshot[name] = [dict(
                            {k: p.get(k) for k in port_keys},
                            upstream_hash=digest(p.get('upstream', '')))
                            for p in rows]
                    elif name == 'solver':
                        snapshot[name] = {k: value.get(k) for k in
                            ['families', 'total_attempts', 'total_solved', 'seen']}
                        snapshot[name]['resources'] = {k: v for k, v in
                            value.get('resources', {}).items() if k not in
                            ['spawns', 'last_error']}
                    elif name == 'queue':
                        snapshot[name] = {k: value.get(k) for k in
                            ['queued', 'running', 'max_len']}
                        snapshot[name]['items'] = [
                            {k: item.get(k) for k in ['port', 'host', 'vendor',
                             'class', 'state', 'waiters', 'age_ms']}
                            for item in value.get('items', [])
                            if item.get('port') in known_ports]
                    else:
                        report = value['report']
                        logs = []
                        for line in report.get('logs', []):
                            try:
                                logs.append(json.loads(line))
                            except (ValueError, TypeError):
                                pass
                        snapshot['log_tail'] = {
                            'rows': len(logs), 'truncated': report.get('truncated'),
                            'first': logs[0].get('ts') if logs else None,
                            'last': logs[-1].get('ts') if logs else None}
                        new_count = 0
                        for row in logs:
                            if row.get('port') not in known_ports:
                                continue
                            key = digest(json.dumps(row, sort_keys=True))
                            if key in seen_events:
                                continue
                            seen_events.add(key)
                            safe = {k: v for k, v in row.items() if k in event_keys}
                            if 'error' in row:
                                safe['error_categories'] = safe_error(row['error'])
                            if 'upstream' in row:
                                safe['upstream_hash'] = digest(row['upstream'])
                            if row.get('msg') in ['port profile rotated',
                                                  'vdns: udp capability verdict changed']:
                                safe.update({k: row[k] for k in ['from', 'to'] if k in row})
                            emit({'event': 'proxy_log', **safe})
                            new_count += 1
                        snapshot['log_tail']['new_job_events'] = new_count
                except Exception as err:
                    errors.append({'source': name, 'type': type(err).__name__})
            try:
                text = fetch('/', parser=True)
                text = re.sub(r'<script[\s\S]*?</script>', '', text)
                text = re.sub(r'\s+', ' ', re.sub(r'<[^>]+>', ' ', text))
                snapshot['ui_counts'] = {
                    label: int(match.group(1)) for label in
                    ['Address changes', 'Of them warm', 'Ports open',
                     'Threads waiting for an egress', 'Set aside']
                    if (match := re.search(re.escape(label) + r'\s+(\d+)', text))}
                snapshot['progress'] = json.loads(fetch(
                    '/api/progress?job=' + str(args.job), parser=True))
            except Exception as err:
                errors.append({'source': 'parser', 'type': type(err).__name__})
            try:
                with sqlite3.connect('file:' + args.database + '?mode=ro', uri=True) as db:
                    db.row_factory = sqlite3.Row
                    snapshot['database'] = {
                        'job': dict(db.execute('select id,created_at,finished_at,'
                            'threads,ports,cooldown_ms,pages,tries,profile_id from jobs '
                            'where id=?', (args.job,)).fetchone()),
                        'states': [dict(r) for r in db.execute('select state,count(*) n '
                            'from queries where job_id=? group by state', (args.job,))],
                        'page_count': db.execute('select count(*) from pages p join '
                            'queries q on q.id=p.query_id where q.job_id=?',
                            (args.job,)).fetchone()[0]}
            except Exception as err:
                errors.append({'source': 'database', 'type': type(err).__name__})
            snapshot['errors'] = errors
            emit(snapshot)
            print(json.dumps({'index': index, 'at': snapshot['at'],
                'ui': snapshot.get('ui_counts'),
                'pages': snapshot.get('database', {}).get('page_count'),
                'tail': snapshot.get('log_tail'), 'errors': errors}), flush=True)
            if index + 1 < args.samples:
                time.sleep(max(0, args.interval - (time.monotonic() - began)))
        emit({'event': 'complete', 'at': dt.datetime.now(dt.timezone.utc).isoformat()})


if __name__ == '__main__':
    main()
