#Requires -Version 5.1
<#
.SYNOPSIS
    InkFrame 一键部署脚本 — Windows
    适用：Windows 10 1809+  /  Windows 11  /  Windows Server 2019+

.DESCRIPTION
    自动安装 Go / Node.js / MySQL / Redis，构建并启动前后端服务。

.PARAMETER Mode
    production（默认）：构建并以后台进程启动
    dev：开发模式，热更新
    stop：停止所有 InkFrame 服务

.EXAMPLE
    .\scripts\deploy-windows.ps1
    .\scripts\deploy-windows.ps1 -Mode dev
    .\scripts\deploy-windows.ps1 -Mode stop
#>

param(
    [ValidateSet("production","dev","stop")]
    [string]$Mode = "production"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# ─── 颜色辅助 ─────────────────────────────────────────────────────────────────
function Write-Info  { param($msg) Write-Host "[INFO]  $msg" -ForegroundColor Green  }
function Write-Warn  { param($msg) Write-Host "[WARN]  $msg" -ForegroundColor Yellow }
function Write-Err   { param($msg) Write-Host "[ERROR] $msg" -ForegroundColor Red    }
function Write-Step  { param($msg) Write-Host "`n━━━ $msg ━━━" -ForegroundColor Blue  }
function Write-Ok    { param($msg) Write-Host "  ✓ $msg"       -ForegroundColor Green }

# ─── 路径 ─────────────────────────────────────────────────────────────────────
$ScriptDir   = Split-Path -Parent $MyInvocation.MyCommand.Path
$BackendDir  = (Resolve-Path "$ScriptDir\..").Path
$RootDir     = (Resolve-Path "$BackendDir\..").Path
$FrontendDir = Join-Path $RootDir "inkframe-frontend"
$LogDir      = Join-Path $BackendDir "logs"
$PidDir      = Join-Path $BackendDir "run"

# ─── 版本 ─────────────────────────────────────────────────────────────────────
$GoVersion      = "1.24.4"
$NodeLtsMajor   = 20
$NodeMinMajor   = 18

# =============================================================================
Write-Host ""
Write-Host "  ╔══════════════════════════════════════╗" -ForegroundColor Cyan
Write-Host "  ║   InkFrame 一键部署  ·  Windows      ║" -ForegroundColor Cyan
Write-Host "  ║   模式: $($Mode.PadRight(30))║" -ForegroundColor Cyan
Write-Host "  ╚══════════════════════════════════════╝" -ForegroundColor Cyan
Write-Host ""

# ─── 执行策略（仅提升当前用户，不影响系统策略） ──────────────────────────────
try {
    Set-ExecutionPolicy -ExecutionPolicy RemoteSigned -Scope CurrentUser -Force -ErrorAction SilentlyContinue
} catch { }

# =============================================================================
# 停止模式
# =============================================================================
if ($Mode -eq "stop") {
    Write-Step "停止 InkFrame 服务"

    foreach ($name in @("inkframe-backend", "inkframe-frontend")) {
        $pidFile = Join-Path $PidDir "$name.pid"
        if (Test-Path $pidFile) {
            $pid = [int](Get-Content $pidFile -ErrorAction SilentlyContinue)
            try {
                Stop-Process -Id $pid -Force -ErrorAction SilentlyContinue
                Write-Ok "已停止 $name (PID $pid)"
            } catch { }
            Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
        }
    }

    # 检查 NSSM 服务
    foreach ($svc in @("InkFrameBackend","InkFrameFrontend")) {
        if (Get-Service -Name $svc -ErrorAction SilentlyContinue) {
            Stop-Service -Name $svc -Force -ErrorAction SilentlyContinue
            Write-Ok "已停止 Windows 服务 $svc"
        }
    }
    Write-Info "服务已停止"
    exit 0
}

New-Item -ItemType Directory -Path $LogDir -Force | Out-Null
New-Item -ItemType Directory -Path $PidDir -Force | Out-Null

# =============================================================================
Write-Step "1 / 8  检查 winget（Windows 包管理器）"
# =============================================================================
$HasWinget = $false
try {
    $null = winget --version 2>&1
    $HasWinget = $true
    Write-Ok "winget 可用"
} catch {
    Write-Warn "winget 不可用，将尝试直接下载安装包"
    Write-Warn "建议在 Microsoft Store 安装 '应用安装程序' 以获得 winget"
}

function Install-WithWinget {
    param([string]$PackageId, [string]$Name)
    Write-Info "安装 $Name via winget..."
    winget install --id $PackageId --silent --accept-package-agreements --accept-source-agreements 2>&1 | Out-Null
}

# 刷新 PATH（安装后立即生效）
function Refresh-Path {
    $machinePath = [System.Environment]::GetEnvironmentVariable("Path", "Machine")
    $userPath    = [System.Environment]::GetEnvironmentVariable("Path", "User")
    $env:Path = "$machinePath;$userPath"
}

# =============================================================================
Write-Step "2 / 8  安装 Go"
# =============================================================================
$GoInstalled = $false
try {
    $gv = (go version 2>&1) -replace "go version go",""
    $major = [int]($gv -split "\.")[0]
    $minor = [int](($gv -split "\.")[1] -replace "[^0-9].*","")
    if ($major -gt 1 -or ($major -eq 1 -and $minor -ge 24)) { $GoInstalled = $true }
} catch { }

if (-not $GoInstalled) {
    if ($HasWinget) {
        Install-WithWinget "GoLang.Go" "Go"
    } else {
        $Arch = if ([System.Environment]::Is64BitOperatingSystem) { "amd64" } else { "386" }
        $GoMsi = "go$GoVersion.windows-$Arch.msi"
        $GoUrl = "https://go.dev/dl/$GoMsi"
        $GoTmp = "$env:TEMP\$GoMsi"
        Write-Info "下载 Go $GoVersion..."
        Invoke-WebRequest -Uri $GoUrl -OutFile $GoTmp -UseBasicParsing -TimeoutSec 300
        Write-Info "安装 Go..."
        Start-Process msiexec.exe -Wait -ArgumentList "/i `"$GoTmp`" /quiet /norestart"
        Remove-Item $GoTmp -Force -ErrorAction SilentlyContinue
    }
    Refresh-Path
}
try   { Write-Ok "Go $(go version)" }
catch { Write-Err "Go 安装失败，请从 https://go.dev/dl/ 手动安装"; exit 1 }

# =============================================================================
Write-Step "3 / 8  安装 Node.js"
# =============================================================================
$NodeInstalled = $false
try {
    $nv = [int]((node --version) -replace "v","" -split "\.")[0]
    if ($nv -ge $NodeMinMajor) { $NodeInstalled = $true }
} catch { }

if (-not $NodeInstalled) {
    if ($HasWinget) {
        Install-WithWinget "OpenJS.NodeJS.LTS" "Node.js LTS"
    } else {
        $NodeUrl = "https://nodejs.org/dist/latest-v$NodeLtsMajor.x/"
        $Page = Invoke-WebRequest -Uri $NodeUrl -UseBasicParsing -TimeoutSec 60
        $NodeMsi = ($Page.Links.href | Where-Object { $_ -match "node-v.*-x64\.msi$" } | Select-Object -First 1)
        $NodeTmp = "$env:TEMP\$NodeMsi"
        Write-Info "下载 Node.js..."
        Invoke-WebRequest -Uri "$NodeUrl$NodeMsi" -OutFile $NodeTmp -UseBasicParsing -TimeoutSec 300
        Start-Process msiexec.exe -Wait -ArgumentList "/i `"$NodeTmp`" /quiet /norestart"
        Remove-Item $NodeTmp -Force -ErrorAction SilentlyContinue
    }
    Refresh-Path
}
try   { Write-Ok "Node.js $(node --version)  npm $(npm --version)" }
catch { Write-Err "Node.js 安装失败，请从 https://nodejs.org 手动安装"; exit 1 }

# =============================================================================
Write-Step "4 / 8  安装 MySQL"
# =============================================================================
$MysqlExe = ""
# 动态搜索已安装的 MySQL（包含未来版本目录）
$MysqlSearchPaths = @(
    "$env:ProgramFiles\MySQL\MySQL Server 8.0\bin\mysql.exe",
    "$env:ProgramFiles\MySQL\MySQL Server 8.4\bin\mysql.exe",
    "C:\MySQL\bin\mysql.exe"
)
# 兼容未来版本：搜索 MySQL Server x.x 下的所有 mysql.exe
$mysqlDirs = Get-ChildItem -Path "$env:ProgramFiles\MySQL" -Filter "mysql.exe" -Recurse -ErrorAction SilentlyContinue
if ($mysqlDirs) { $MysqlExe = $mysqlDirs[0].FullName }

if (-not $MysqlExe) {
    foreach ($p in $MysqlSearchPaths) {
        if (Test-Path $p) { $MysqlExe = $p; break }
    }
}
if (-not $MysqlExe) {
    try { $MysqlExe = (Get-Command mysql -ErrorAction Stop).Source } catch { }
}

if (-not $MysqlExe) {
    if ($HasWinget) {
        Install-WithWinget "Oracle.MySQL" "MySQL"
        Refresh-Path
    } else {
        Write-Warn "请手动安装 MySQL: https://dev.mysql.com/downloads/installer/"
        Write-Warn "安装后重新运行本脚本"
        pause; exit 1
    }
    # 安装后再次搜索
    $mysqlDirs = Get-ChildItem -Path "$env:ProgramFiles\MySQL" -Filter "mysql.exe" -Recurse -ErrorAction SilentlyContinue
    if ($mysqlDirs) { $MysqlExe = $mysqlDirs[0].FullName }
    if (-not $MysqlExe) {
        try { Refresh-Path; $MysqlExe = (Get-Command mysql -ErrorAction Stop).Source } catch { }
    }
}
Write-Ok "MySQL: $MysqlExe"

# 启动 MySQL 服务（搜索所有已知服务名）
$MysqlServiceStarted = $false
foreach ($svc in @("MySQL80","MySQL","MySQL84","MySQL90")) {
    if (Get-Service -Name $svc -ErrorAction SilentlyContinue) {
        Start-Service -Name $svc -ErrorAction SilentlyContinue
        Write-Ok "MySQL 服务已启动 ($svc)"
        $MysqlServiceStarted = $true
        break
    }
}
if (-not $MysqlServiceStarted) {
    Write-Warn "未找到已知 MySQL 服务名，请手动确认 MySQL 已运行"
}

# =============================================================================
Write-Step "5 / 8  安装 Redis"
# =============================================================================
$RedisExe = ""
try { $RedisExe = (Get-Command redis-server -ErrorAction Stop).Source } catch { }
if (-not $RedisExe) {
    foreach ($p in @(
        "$env:ProgramFiles\Redis\redis-server.exe",
        "C:\Redis\redis-server.exe"
    )) {
        if (Test-Path $p) { $RedisExe = $p; break }
    }
}

if (-not $RedisExe) {
    if ($HasWinget) {
        # 尝试 winget 安装（Redis.Redis 是官方维护的 Windows 包）
        Write-Info "尝试通过 winget 安装 Redis..."
        try {
            winget install --id Redis.Redis --silent --accept-package-agreements --accept-source-agreements 2>&1 | Out-Null
            Refresh-Path
            try { $RedisExe = (Get-Command redis-server -ErrorAction Stop).Source } catch { }
        } catch { }
    }
}

if (-not $RedisExe) {
    # microsoftarchive/redis 已于 2016 年停止维护，不再使用。
    # 请使用以下方式之一安装 Redis：
    Write-Err "未找到 Redis，请手动安装后重新运行脚本："
    Write-Warn "  方案1 (推荐): winget install Redis.Redis"
    Write-Warn "  方案2: 安装 WSL2，在 Linux 子系统中运行 Redis"
    Write-Warn "  方案3: 下载 Memurai（Redis 兼容）: https://www.memurai.com/get-memurai"
    pause; exit 1
}
Write-Ok "Redis: $RedisExe"

# 启动 Redis（后台）
$RedisPing = $false
try { $RedisPing = ((redis-cli ping 2>&1) -eq "PONG") } catch { }
if (-not $RedisPing) {
    Start-Process -FilePath $RedisExe -WindowStyle Hidden -PassThru | Out-Null
    Start-Sleep 1
    Write-Ok "Redis 已启动"
} else {
    Write-Ok "Redis 已运行"
}

# =============================================================================
Write-Step "6 / 8  配置 config.yaml"
# =============================================================================
$ConfigFile    = Join-Path $BackendDir "config.yaml"
$ConfigExample = Join-Path $BackendDir "config.example.yaml"

if (-not (Test-Path $ConfigFile)) {
    Copy-Item $ConfigExample $ConfigFile
    Write-Info "已从 config.example.yaml 创建 config.yaml"
} else {
    # 重复运行时备份现有配置
    $backupPath = "$ConfigFile.bak.$([DateTimeOffset]::UtcNow.ToUnixTimeSeconds())"
    Copy-Item $ConfigFile $backupPath
    Write-Info "已备份现有 config.yaml -> $backupPath"
}

Write-Host ""
Write-Host "请输入数据库配置（直接回车使用括号内默认值）：" -ForegroundColor Cyan

function Read-Default { param([string]$prompt, [string]$default)
    $val = Read-Host "  $prompt [$default]"
    if ([string]::IsNullOrWhiteSpace($val)) { $default } else { $val }
}

$DbHost  = Read-Default "MySQL 主机" "localhost"
$DbPort  = Read-Default "MySQL 端口" "3306"
$DbName  = Read-Default "数据库名称" "inkframe"
$DbUser  = Read-Default "用户名" "root"
$DbPassSS  = Read-Host "  密码          (空=无密码)" -AsSecureString
$DbPassPlain = [Runtime.InteropServices.Marshal]::PtrToStringAuto(
    [Runtime.InteropServices.Marshal]::SecureStringToBSTR($DbPassSS))
$DbPassSS.Dispose()
$BePort  = Read-Default "后端端口" "8080"
$FePort  = Read-Default "前端端口" "3000"

# 替换 config.yaml（使用正则，注意转义密码中的特殊字符）
$cfg = Get-Content $ConfigFile -Raw
$cfg = $cfg -replace 'host: "localhost"\s+# \[必填\]', "host: `"$([Regex]::Escape($DbHost) -replace '\\', '')`""
# 使用简单的字符串替换（[Regex]::Escape 处理特殊字符）
$cfg = $cfg.Replace('host: "localhost"        # [必填]', "host: `"$DbHost`"")
$cfg = $cfg.Replace('port: 3306',              "port: $DbPort")
$cfg = $cfg.Replace('database: "inkframe"     # [必填]', "database: `"$DbName`"")
$cfg = $cfg.Replace('username: "root"         # [必填]', "username: `"$DbUser`"")
$cfg = $cfg.Replace('password: ""             # [必填]', "password: `"$DbPassPlain`"")
$cfg = $cfg.Replace('port: 8080',              "port: $BePort")
if ($Mode -eq "production") {
    Add-Type -AssemblyName System.Security
    $bytes = New-Object byte[] 48
    [System.Security.Cryptography.RNGCryptoServiceProvider]::Create().GetBytes($bytes)
    $jwt = [Convert]::ToBase64String($bytes)
    $cfg = $cfg.Replace(
        'jwt_secret: "inkframe-dev-secret-change-in-production-2024"',
        "jwt_secret: `"$jwt`"")
    $jwt = $null
}
$cfg | Set-Content $ConfigFile -Encoding UTF8
Write-Ok "config.yaml 已更新"

# 创建数据库：密码通过 --defaults-extra-file 临时文件传入，不在进程参数中暴露
$TmpCnfFile = $null
$MysqlExtraArgs = @("-h", $DbHost, "-P", $DbPort, "-u", $DbUser)
if ($DbPassPlain) {
    $TmpCnfFile = [System.IO.Path]::GetTempFileName()
    "[client]`npassword=$DbPassPlain" | Set-Content -Path $TmpCnfFile -Encoding ASCII
    # 临时文件权限收紧（仅当前用户可读）
    $acl = Get-Acl $TmpCnfFile
    $acl.SetAccessRuleProtection($true, $false)
    $rule = New-Object System.Security.AccessControl.FileSystemAccessRule(
        [System.Security.Principal.WindowsIdentity]::GetCurrent().Name,
        "FullControl", "Allow")
    $acl.AddAccessRule($rule)
    Set-Acl $TmpCnfFile $acl
    $MysqlExtraArgs = @("--defaults-extra-file=$TmpCnfFile") + $MysqlExtraArgs
}

$CreateDb = "CREATE DATABASE IF NOT EXISTS ``$DbName`` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"
try {
    $result = & $MysqlExe @MysqlExtraArgs -e $CreateDb 2>&1
    Write-Ok "数据库 '$DbName' 就绪"
} catch {
    Write-Warn "数据库创建失败，请手动执行：$CreateDb"
}

if ($TmpCnfFile -and (Test-Path $TmpCnfFile)) {
    Remove-Item $TmpCnfFile -Force -ErrorAction SilentlyContinue
}

# 使用后清理敏感数据
$DbPassPlain = [string]::new('*', [Math]::Max(1, $DbPassPlain.Length))
$DbPassPlain = $null
[GC]::Collect()

# =============================================================================
Write-Step "7 / 8  构建后端"
# =============================================================================
Set-Location $BackendDir
Write-Info "下载 Go 依赖..."
go mod download
if ($LASTEXITCODE -ne 0) { Write-Err "go mod download 失败，请检查网络或 go.sum 文件"; exit 1 }
go mod tidy
if ($LASTEXITCODE -ne 0) { Write-Err "go mod tidy 失败"; exit 1 }

if ($Mode -eq "production") {
    Write-Info "编译后端..."
    New-Item -ItemType Directory -Path "bin" -Force | Out-Null
    go build -ldflags "-s -w" -o "bin\inkframe-backend.exe" ".\cmd\server"
    if ($LASTEXITCODE -ne 0) { Write-Err "后端编译失败"; exit 1 }
    Write-Ok "后端: $BackendDir\bin\inkframe-backend.exe"
} else {
    Write-Ok "开发模式：跳过编译"
}

# =============================================================================
Write-Step "8 / 8  安装前端依赖、构建并启动服务"
# =============================================================================
if (-not (Test-Path $FrontendDir)) {
    Write-Err "前端目录不存在: $FrontendDir"
    Write-Err "请确认 inkframe-frontend 与 inkframe-backend 在同一父目录下"
    exit 1
}

Set-Location $FrontendDir
Write-Info "安装 npm 依赖..."
npm install --prefer-offline 2>&1 | Select-Object -Last 5 | ForEach-Object { Write-Host "  $_" }
if ($LASTEXITCODE -ne 0) { Write-Err "npm install 失败，请检查 Node.js 版本或网络"; exit 1 }

if ($Mode -eq "production") {
    Write-Info "构建前端..."
    $env:NUXT_PUBLIC_API_BASE = "http://localhost:$BePort/api/v1"
    npm run build 2>&1 | Select-Object -Last 10 | ForEach-Object { Write-Host "  $_" }
    if ($LASTEXITCODE -ne 0) { Write-Err "前端构建失败，请查看以上输出"; exit 1 }
    Write-Ok "前端构建完成"
} else {
    Write-Ok "开发模式：跳过构建"
}

# ── 启动服务 ──────────────────────────────────────────────────────────────────
Set-Location $BackendDir

# 停掉旧进程
foreach ($name in @("inkframe-backend","inkframe-frontend")) {
    $pf = Join-Path $PidDir "$name.pid"
    if (Test-Path $pf) {
        $oldPid = [int](Get-Content $pf -ErrorAction SilentlyContinue)
        Stop-Process -Id $oldPid -Force -ErrorAction SilentlyContinue
        Remove-Item $pf -Force -ErrorAction SilentlyContinue
    }
}

$BackendLog  = Join-Path $LogDir "backend.log"
$FrontendLog = Join-Path $LogDir "frontend.log"

if ($Mode -eq "production") {
    # 后端
    $bProc = Start-Process -FilePath ".\bin\inkframe-backend.exe" `
        -WorkingDirectory $BackendDir `
        -RedirectStandardOutput $BackendLog `
        -RedirectStandardError  $BackendLog `
        -WindowStyle Hidden -PassThru
    $bProc.Id | Set-Content (Join-Path $PidDir "inkframe-backend.pid")
    Write-Ok "后端已启动 (PID $($bProc.Id), 日志: logs\backend.log)"

    # 前端
    $env:PORT = $FePort
    $fProc = Start-Process -FilePath "node" `
        -ArgumentList ".output\server\index.mjs" `
        -WorkingDirectory $FrontendDir `
        -RedirectStandardOutput $FrontendLog `
        -RedirectStandardError  $FrontendLog `
        -WindowStyle Hidden -PassThru
    $fProc.Id | Set-Content (Join-Path $PidDir "inkframe-frontend.pid")
    Write-Ok "前端已启动 (PID $($fProc.Id), 日志: logs\frontend.log)"

} else {
    # 开发模式：用 Windows Terminal 或 cmd 打开新窗口
    $wtExe = (Get-Command wt -ErrorAction SilentlyContinue)?.Source
    if ($wtExe) {
        Start-Process wt -ArgumentList `
            "new-tab --title Backend cmd /k `"cd /d `"$BackendDir`" && go run .\cmd\server`"",
            "; new-tab --title Frontend cmd /k `"cd /d `"$FrontendDir`" && npm run dev`""
        Write-Ok "已在 Windows Terminal 新标签页中启动（开发模式）"
    } else {
        # 后端
        $bProc = Start-Process cmd -ArgumentList "/k cd /d `"$BackendDir`" && go run .\cmd\server" -PassThru
        $bProc.Id | Set-Content (Join-Path $PidDir "inkframe-backend.pid")
        # 前端
        $fProc = Start-Process cmd -ArgumentList "/k cd /d `"$FrontendDir`" && npm run dev" -PassThru
        $fProc.Id | Set-Content (Join-Path $PidDir "inkframe-frontend.pid")
        Write-Ok "已在独立 cmd 窗口中启动"
    }
}

# 等待后端就绪（最多 30 秒）
Write-Info "等待后端就绪..."
$BeReady = $false
for ($i = 0; $i -lt 30; $i++) {
    try {
        $r = Invoke-WebRequest -Uri "http://localhost:$BePort/health" `
            -UseBasicParsing -TimeoutSec 1 -ErrorAction SilentlyContinue
        if ($r.StatusCode -eq 200) { $BeReady = $true; break }
    } catch { }
    Start-Sleep 1
}
if (-not $BeReady) {
    Write-Warn "后端在 30 秒内未响应 /health，请检查日志: $BackendLog"
}

Write-Host ""
Write-Host "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" -ForegroundColor Green
Write-Host "  ✅  InkFrame 部署成功！"                  -ForegroundColor Green
Write-Host "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" -ForegroundColor Green
Write-Host "  前端地址: http://localhost:$FePort"        -ForegroundColor Cyan
Write-Host "  后端地址: http://localhost:$BePort"        -ForegroundColor Cyan
Write-Host "  日志目录: $LogDir"                         -ForegroundColor Cyan
if ($Mode -eq "production") {
    Write-Host "  停止服务: .\scripts\deploy-windows.ps1 -Mode stop" -ForegroundColor Cyan
}
Write-Host ""
