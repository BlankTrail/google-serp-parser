param(
    [Parameter(Mandatory = $true)][string]$InputPath,
    [string]$ProxyLog,
    [string]$EventsOutput
)

$ErrorActionPreference = 'Stop'
$probeRows = @(Get-Content -LiteralPath $InputPath | ForEach-Object {
    # A reader can catch the last line while the producer is writing it.
    try { ConvertFrom-Json -InputObject $_ -ErrorAction Stop } catch { }
})
$probeStart = $probeRows | Where-Object event -eq 'start' | Select-Object -First 1
if ($null -eq $probeStart) { throw 'No start record in the probe output' }
$probeRequests = @($probeRows | Where-Object event -eq 'request')
$probeWarm = @($probeRequests | Where-Object { -not $_.cold })
$probeIPs = @($probeRows | Where-Object { $_.event -eq 'exit_ip' -and $_.available } | Select-Object -ExpandProperty ip -Unique)
$probeSummary = [ordered]@{
    started = $probeStart.at
    port = $probeStart.port
    build = $probeStart.build
    gap_seconds = $probeStart.gap_seconds
    completed = @($probeRows | Where-Object event -eq 'summary').Count -gt 0
    requests = $probeRequests.Count
    warm_requests = $probeWarm.Count
    warm_usable = @($probeWarm | Where-Object { $_.class -in @('serp', 'empty') }).Count
    warm_solve_attempts = ($probeWarm | Measure-Object solves_delta -Sum).Sum
    warm_solved = ($probeWarm | Measure-Object solved_delta -Sum).Sum
    warm_searches_with_solver = @($probeWarm | Where-Object { $_.solves_delta -gt 0 }).Count
    last_request_age_seconds = ($probeRequests | Select-Object -Last 1).age_seconds
    minimum_warm_idle_seconds = ($probeWarm | Measure-Object idle_seconds -Minimum).Minimum
    identity_changed = @($probeRequests | Where-Object { $_.port_identity_stable -ne $true }).Count -gt 0
    counters_isolated_at_snapshots = @($probeRequests | Where-Object { $_.counters_isolated -ne $true }).Count -eq 0
    observed_exit_ips = $probeIPs
    request_seconds = @($probeRequests | ForEach-Object { [math]::Round($_.seconds, 3) })
}

if ($ProxyLog) {
    $probeBeginning = [DateTimeOffset]$probeStart.at
    $probeLast = $probeRequests | Select-Object -Last 1
    $probeEnd = if ($null -ne $probeLast) {
        ([DateTimeOffset]$probeLast.at).AddSeconds($probeLast.seconds)
    } else { [DateTimeOffset]::Now }
    $probeEvents = @(Get-Content -LiteralPath $ProxyLog | ForEach-Object {
        try { $probeEvent = ConvertFrom-Json -InputObject $_ -ErrorAction Stop } catch { return }
        if ($probeEvent.port -ne $probeStart.port -or -not $probeEvent.ts) { return }
        $probeAt = [DateTimeOffset]$probeEvent.ts
        if ($probeAt -lt $probeBeginning -or $probeAt -gt $probeEnd) { return }
        if ($probeEvent.msg -notmatch '(рядовая подстановка кук|challenge detected|re-issued with solved cookies|личность порта на выходе|fast-lane submit returned|cb-lane exact response classified|browser context created|browser context disposed|port profile rotated|profile rotated via API|upstream changed via API|port closed|port opened)') { return }
        # Explicit allowlist: no cookie values, credentials, bodies or URLs with
        # challenge tokens are copied from the service's diagnostic log.
        $probeEvent | Select-Object ts, msg, port, host, injected, names, status,
            new_status, vendor, changed, action, waited, lane, lived
    })
    $probeSummary.port_challenge_events = @($probeEvents | Where-Object msg -eq 'solver: challenge detected').Count
    $probeSummary.port_rotation_events = @($probeEvents | Where-Object {
        $_.msg -match '(port profile rotated|profile rotated via API|upstream changed via API)'
    }).Count
    $probeSummary.google_cookie_injections = @($probeEvents | Where-Object {
        $_.msg -eq 'solver: рядовая подстановка кук' -and $_.host -match '(^|\.)google\.com$' -and $_.injected -gt 0
    }).Count
    if ($EventsOutput) {
        if (Test-Path -LiteralPath $EventsOutput) { throw 'Events output already exists' }
        $probeEvents | ForEach-Object { ConvertTo-Json -InputObject $_ -Depth 5 -Compress } |
            Set-Content -LiteralPath $EventsOutput -Encoding utf8
    }
}
ConvertTo-Json -InputObject $probeSummary -Depth 5
