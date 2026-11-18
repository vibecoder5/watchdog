[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ConfigFile,
    [string]$Server = '195.133.76.98',
    [string]$User = 'root',
    [ValidateRange(1, 65535)][int]$Port = 22,
    [string]$Domain = 'watchdog.u-agent.ru',
    [ValidateSet('amd64', 'arm64')][string]$Architecture = 'amd64'
)

$ErrorActionPreference = 'Stop'
if ($Server -notmatch '^[a-zA-Z0-9.-]+$' -or $Server.StartsWith('-')) { throw 'Invalid SSH server.' }
if ($User -notmatch '^[a-zA-Z_][a-zA-Z0-9_-]*$') { throw 'Invalid SSH user.' }
if ($Domain -notmatch '^[a-zA-Z0-9][a-zA-Z0-9.-]*[a-zA-Z0-9]$') { throw 'Invalid domain syntax.' }
foreach ($command in @('ssh', 'scp')) { Get-Command $command -ErrorAction Stop | Out-Null }
$projectDir = Split-Path -Parent $PSScriptRoot
$binaryPath = Join-Path $projectDir "bin/watchdog-linux-$Architecture"
$configPath = (Resolve-Path -LiteralPath $ConfigFile).ProviderPath
if (-not (Test-Path -LiteralPath $configPath -PathType Leaf)) { throw 'ConfigFile must be a file.' }
if (-not (Test-Path -LiteralPath $binaryPath -PathType Leaf)) { throw "Build the Linux $Architecture binary first: $binaryPath" }
$configText = [IO.File]::ReadAllText($configPath)
if ($configText -notmatch '(?m)^\s*WATCHDOG_PASSWORD_HASH\s*=' -or $configText.Contains('REPLACE_WITH_GENERATED_BCRYPT_HASH')) {
    throw 'Set WATCHDOG_PASSWORD_HASH in ConfigFile before deploying.'
}
$configText = $null
$target = "$User@$Server"
$sshArgs = @('-p', "$Port", '-o', 'ConnectTimeout=15')
$remoteDir = $null
try {
    $remoteOutput = & ssh @sshArgs $target 'umask 077; mktemp -d /tmp/watchdog-deploy.XXXXXXXXXX'
    if ($LASTEXITCODE -ne 0) { throw 'Cannot create a private upload directory on the server.' }
    $remoteDir = ($remoteOutput -join "`n").Trim()
    if ($remoteDir -notmatch '^/tmp/watchdog-deploy\.[a-zA-Z0-9]+$') { throw 'Unexpected upload directory returned by SSH.' }
    $files = @{
        'watchdog' = $binaryPath
        'watchdog.env' = $configPath
        'install.sh' = (Join-Path $PSScriptRoot 'install.sh')
        'watchdog.service' = (Join-Path $PSScriptRoot 'watchdog.service')
        'watchdog.env.example' = (Join-Path $PSScriptRoot 'watchdog.env.example')
        'deploy-remote.sh' = (Join-Path $PSScriptRoot 'deploy-remote.sh')
    }
    foreach ($name in $files.Keys) {
        & scp -P "$Port" -o ConnectTimeout=15 -- $files[$name] "${target}:${remoteDir}/$name"
        if ($LASTEXITCODE -ne 0) { throw "Upload failed: $name" }
    }
    # All interpolated shell values above are restricted to safe characters.
    # -t allows sudo to prompt for non-root users. No DNS or HTTP domain probe is made.
    $privilege = if ($User -eq 'root') { '' } else { 'sudo ' }
    & ssh -t @sshArgs $target "${privilege}sh '$remoteDir/deploy-remote.sh' '$remoteDir' '$Domain'"
    if ($LASTEXITCODE -ne 0) { throw 'Remote deployment failed. See the server error above.' }
}
finally {
    if ($remoteDir -match '^/tmp/watchdog-deploy\.[a-zA-Z0-9]+$') {
        # Delete only the explicit upload files, never recurse through a computed path.
        $cleanup = "rm -f -- '$remoteDir/watchdog.env' '$remoteDir/watchdog' '$remoteDir/install.sh' '$remoteDir/watchdog.service' '$remoteDir/watchdog.env.example' '$remoteDir/deploy-remote.sh'; rmdir -- '$remoteDir'"
        & ssh @sshArgs -o BatchMode=yes $target $cleanup
        if ($LASTEXITCODE -ne 0) { Write-Warning "Remove the private upload directory manually: $remoteDir" }
    }
}
