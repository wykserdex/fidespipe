# windows-test-guard.ps1 — прогон тестов по пакетам с уликами, переживающими отмену джоба.
#
# Зачем он существует (подробности и все проверенные факты — docs/windows-ci-hang.md):
#
#  1. НИКАКИХ ПАЙПОВ между нами и `go`. Захват вывода — редирект в ФАЙЛ, который
#     делает cmd.exe (Windows) или sh (Unix). Это не стилистика: у .NET
#     Process.WaitForExit() БЕЗ аргумента нет предела, и он ждёт не только выхода
#     процесса, но и EOF перенаправленных пайпов, поэтому любой потомок, удержавший
#     копию write-конца, превращает «дождаться пакета» в «стоять вечно». Файловый
#     хендл, который кто-то держит, никого не блокирует.
#
#  2. У КАЖДОГО ожидания есть предел: `go list`, пакет, убийство дерева.
#
#  3. CENSUS: вокруг каждого пакета снимается снимок таблицы процессов. Всё, что
#     появилось и осталось жить ПОСЛЕ пакета, печатается с командной строкой,
#     родителем и родословной. Это отвечает на вопрос «какой процесс переживает
#     тест» на ЛЮБОМ прогоне, в том числе зелёном — зависание для этого не нужно.
#     DETACHED — процесс оторван от нашего дерева (родитель мёртв либо путь до нас
#     разорван), SURVIVOR — жив, но всё ещё наш потомок.
#
#  4. Всё, что печатается, дублируется в GITHUB_STEP_SUMMARY: он переживает отмену
#     джоба, тогда как лог шага не отдаётся (BlobNotFound), а артефакт не
#     выгружается — выгружает его следующий шаг, до которого при зависании не
#     доходит.
#
# Запуск вручную, без CI (из корня репозитория; $env:TEMP\gotest — каталог логов):
#   pwsh ./tools/ci/windows-test-guard.ps1 -LogDir $env:TEMP\gotest -RepoDir .
# или так же, как это делает CI, с внешней сторожкой:
#   pwsh ./tools/ci/run-guard-with-watchdog.ps1 -LogDir $env:TEMP\gotest `
#     -BudgetSec 1200 -PerPackageSec 540 -GoTimeoutSec 480 -KillLeakedDescendants
#
# Лестница подписей в summary (её и надо читать первой):
#   нет строки «guard start»            → встало ДО нас (шаг/шелл/раннер);
#   есть «guard start», нет «пакетов:»   → встал `go list ./...` (или старт воркера);
#   есть «=== pkg ===»                   → встало на этом пакете;
#   есть строки DETACHED/SURVIVOR        → вот кто пережил пакет.

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$LogDir,
    [int]$BudgetSec = 1200,
    [int]$PerPackageSec = 540,
    [int]$GoTimeoutSec = 480,
    [int]$ListTimeoutSec = 120,
    [int]$CensusTimeoutSec = 10,
    # Каталог репозитория (там, где go.mod). По умолчанию — текущий; задавать
    # явно, если сторож вызывают не из корня репозитория.
    [string]$RepoDir = '.',
    # Убивать найденных оторванных потомков. Зачем: выживший потомок может
    # держать пайпы самого раннера, и тогда «шаг» никогда не завершается, а его
    # лог не финализируется. Это не лечение причины, а способ не потерять улики.
    [switch]$KillLeakedDescendants,
    [string]$StepSummary = $env:GITHUB_STEP_SUMMARY
)

$ErrorActionPreference = 'Continue'
$script:IsWin = ($env:OS -eq 'Windows_NT')
$script:Start = Get-Date
$script:NoticeSeq = 0
$script:SummaryPath = $StepSummary
$resolved = Resolve-Path -LiteralPath $RepoDir -ErrorAction SilentlyContinue
$script:RepoDir = if ($resolved) { $resolved.Path } else { $RepoDir }

function Write-Line {
    param([string]$Text, [switch]$Notice, [string]$NoticeText)
    $stamp = (Get-Date).ToString('HH:mm:ss.fff')
    $elapsed = [int]((Get-Date) - $script:Start).TotalSeconds
    $line = "[{0} +{1,4}s] {2}" -f $stamp, $elapsed, $Text
    Write-Host $line
    if ($script:SummaryPath) {
        Add-Content -Path $script:SummaryPath -Value $line -ErrorAction SilentlyContinue
    }
    if ($Notice) {
        # В аннотацию кладём компактную форму: GitHub склеивает все аннотации с
        # одинаковым title в одну и ОБРЕЗАЕТ её (проверено: 21 строка → 10, и
        # обрезался хвост вместе с итогом). Поэтому в аннотацию идёт короткий
        # текст, а полная строка с таймстемпом остаётся в логе и summary.
        # Формат-оператор, а не интерполяция: в PowerShell "{$elapsed}s $Text"
        # печатает фигурные скобки литералом, и в аннотацию уезжал "{0}s ...".
        $n = if ($NoticeText) { $NoticeText } else { '{0}s {1}' -f $elapsed, $Text }
        Write-Notice -Text $n
    }
}

# Аннотация, а не только строка summary. Проверено на отменённом джобе
# 2026-09-28: содержимое GITHUB_STEP_SUMMARY при отмене ТЕРЯЕТСЯ и через REST
# не читается даже на зелёном прогоне, а лог джоба отдаётся не всегда
# (на том же инциденте — "log not found"). Workflow-команда из stdout попадает
# в объект check-run и переживает отмену: /check-runs/{id}/annotations отдаёт
# её после force-kill. Поэтому вердикт census обязан идти сюда.
# Всё сообщение кладём в message (после `::`), а title держим константой:
# в свойствах до `::` запятая и двоеточие требуют экранирования.
function Write-Notice {
    param([string]$Text, [ValidateSet('warning', 'error', 'notice')][string]$Level = 'warning')
    $esc = $Text -replace '%', '%25' -replace "`r", '%0D' -replace "`n", '%0A'
    # title уникален НА КАЖДУЮ строку, иначе GitHub склеивает все аннотации с
    # одинаковым title в один объект и оставляет только 10 — проверено дважды
    # (21 аннотация → 10 строк, потом 19 → те же 10, причём отбрасывались
    # самые свежие). Уникальный title делает склейку невозможной, и все строки
    # читаются из /annotations. Префикс census/ оставлен для выборки.
    $script:NoticeSeq = [int]$script:NoticeSeq + 1
    Write-Output ("::{0} title=census/{1}::{2}" -f $Level, $script:NoticeSeq, $esc)
}

# --- снимок таблицы процессов --------------------------------------------------
# Что считать «похожим на наш». Список покрывает ИНСТРУМЕНТЫ, которые порождает
# набор тестов, а не только очевидное: регрессия на предел ожидания запускает
# `cmd /c start /b ping` (Windows) или `sh -c "sleep 60 &"` (POSIX), и раньше
# `ping.exe`/`cmd.exe` не попадали ни под одну ветку — процесс утекал, а
# -KillLeakedDescendants его не трогал, потому что решает по rel=1.
# Проверено на реальных именах: ping.exe и cmd.exe были False, python.exe/git.exe
# были True, svchost.exe остаётся False.
# В шаблоне командной строки НЕТ `"`: он ловил любой процесс, у которого в
# командной строке есть кавычка (напр. firefox -contentproc "..."), и такой
# процесс попадал в rel=1, а значит его можно было УБИТЬ — при ручном запуске
# census на своём компьютере это значило бы выбить браузер. Кавычки в `cmd /c
# "..."` ловить не нужно: сам cmd уже в списке имён.
$script:InterestingCmd = 'python|main\.py|\.test(\.exe)?(\s|$)|taskkill|\bsleep\b|\bping\b'
# Имена сверяем без учёта расширения: на Windows путь оканчивается на .exe,
# на POSIX — нет, а список инструментов один и тот же.
# conhost здесь нет намеренно: он всегда потомок консольного процесса, а тот уже
# в списке, и включать его — значит убивать каллотерапию рядом с целью.
$script:InterestingNames = @(
    'python', 'git', 'go', 'taskkill', 'ping', 'cmd', 'powershell', 'pwsh',
    'bash', 'sh', 'sleep', 'curl', 'ssh', 'node', 'dotnet', 'java'
)
$script:Ours = @{}

function Ensure-Toolhelp {
    if (-not $script:IsWin) { return }
    if ('Fidespipe.Toolhelp' -as [type]) { return }
    # -TypeDefinition, а не -MemberDefinition: using-директивы легальны только
    # на верхнем уровне исходника. Тип тот же — Fidespipe.Toolhelp.
    Add-Type -TypeDefinition @'
        using System;
        using System.Collections.Generic;
        using System.Runtime.InteropServices;

        namespace Fidespipe { public static class Toolhelp {

        [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
        public struct PROCESSENTRY32W {
            public uint dwSize;
            public uint cntUsage;
            public uint th32ProcessID;
            public IntPtr th32DefaultHeapID;
            public uint th32ModuleID;
            public uint cntThreads;
            public uint th32ParentProcessID;
            public int pcPriClassBase;
            public uint dwFlags;
            [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 260)]
            public string szExeFile;
        }

        public class ProcEntry {
            public int Pid;
            public int Ppid;
            public string Name;
        }

        [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
        public static extern IntPtr CreateToolhelp32Snapshot(uint dwFlags, uint th32ProcessID);

        [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
        public static extern bool Process32FirstW(IntPtr hSnapshot, ref PROCESSENTRY32W lppe);

        [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
        public static extern bool Process32NextW(IntPtr hSnapshot, ref PROCESSENTRY32W lppe);

        [DllImport("kernel32.dll", SetLastError = true)]
        public static extern bool CloseHandle(IntPtr hObject);

        public static List<ProcEntry> Snapshot() {
            var list = new List<ProcEntry>();
            IntPtr snap = CreateToolhelp32Snapshot(0x00000002, 0);
            if (snap == IntPtr.Zero || snap == new IntPtr(-1)) {
                return list;
            }
            try {
                PROCESSENTRY32W pe = new PROCESSENTRY32W();
                pe.dwSize = (uint)Marshal.SizeOf(typeof(PROCESSENTRY32W));
                if (Process32FirstW(snap, ref pe)) {
                    do {
                        list.Add(new ProcEntry {
                            Pid = (int)pe.th32ProcessID,
                            Ppid = (int)pe.th32ParentProcessID,
                            Name = pe.szExeFile ?? ""
                        });
                    } while (Process32NextW(snap, ref pe));
                }
            } finally {
                CloseHandle(snap);
            }
            return list;
        }
        } }
'@
}

function Get-Census {
    $table = @{}
    if ($script:IsWin) {
        try {
            Ensure-Toolhelp
            foreach ($p in [Fidespipe.Toolhelp]::Snapshot()) {
                $table[[int]$p.Pid] = [pscustomobject]@{
                    Pid = [int]$p.Pid; Ppid = [int]$p.Ppid
                    Name = $p.Name; Cmd = ''
                }
            }
            return $table
        } catch {
            foreach ($p in Get-Process -ErrorAction SilentlyContinue) {
                $table[[int]$p.Id] = [pscustomobject]@{
                    Pid = [int]$p.Id; Ppid = -1; Name = $p.ProcessName; Cmd = ''
                }
            }
            return $table
        }
    }
    $ps = Get-Command ps -ErrorAction SilentlyContinue
    if (-not $ps) { return $table }
    $out = & ps -eo 'pid=,ppid=,lstart=,args=' 2>$null
    foreach ($line in $out) {
        if (-not $line) { continue }
        if ($line -match '^\s*(\d+)\s+(\d+)\s+(.{24})\s*(.*)$') {
            $table[[int]$Matches[1]] = [pscustomobject]@{
                Pid = [int]$Matches[1]; Ppid = [int]$Matches[2]
                Name = ''; Cmd = $Matches[4]
            }
        }
    }
    return $table
}

function Get-CensusTimed {
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $t = Get-Census
    $sw.Stop()
    if ($sw.ElapsedMilliseconds -gt 5000) {
        Write-Line ("WARN: снимок процессов занял {0} мс — WMI тормозит, census может врать" -f $sw.ElapsedMilliseconds)
    }
    return $t
}

function Format-Ancestry {
    param([hashtable]$Table, [int]$Ppid, [int]$Depth = 4)
    $parts = @()
    $cur = $Ppid
    while ($Depth-- -gt 0 -and $cur -gt 0) {
        if ($Table.ContainsKey($cur)) {
            $a = $Table[$cur]
            $nm = if ($a.Name) { $a.Name } else { ($a.Cmd -split '\s+')[0] }
            $parts += ("{0}({1})" -f $nm, $cur)
            $cur = $a.Ppid
        } else {
            $parts += ("pid $cur (умер)")
            break
        }
    }
    return ($parts -join ' <- ')
}

# Оторван ли процесс от нашего дерева: поднимаемся по родителям; встретили наш
# pid — процесс наш; встретили мёртвый pid (на Windows ppid покойного остаётся в
# таблице как есть) или дошли до init — процесс оторван. На POSIX сирота
# переподчиняется init, поэтому одного «родителя нет в таблице» недостаточно.
function Test-Detached {
    param([hashtable]$Table, $Proc)
    $cur = [int]$Proc.Ppid
    $depth = 0
    while ($depth++ -lt 10 -and $cur -gt 1) {
        if ($script:Ours.ContainsKey($cur)) { return $false }
        if (-not $Table.ContainsKey($cur)) { return $true }
        $cur = [int]$Table[$cur].Ppid
    }
    return $true
}

function Test-Interesting {
    param($Proc)
    if ($Proc.Cmd -and ($Proc.Cmd -match $script:InterestingCmd)) { return $true }
    if (-not $Proc.Name) { return $false }
    # Имя сравниваем без расширения: PING.EXE, ping.exe и ping должны совпасть
    # с одним и тем же элементом списка, иначе правило зависит от ОС.
    $base = [System.IO.Path]::GetFileNameWithoutExtension($Proc.Name)
    if (-not $base) { $base = $Proc.Name }
    # Go-бинарники тестов (core.test.exe, plugin.test.exe) раньше ловились по
    # командной строке через InterestingCmd. После перехода census на Toolhelp
    # командной строки нет (Cmd = ''), поэтому ловим их по суффиксу имени.
    if ($base.ToLowerInvariant() -match '\.test$') { return $true }
    return ($script:InterestingNames -contains $base.ToLowerInvariant())
}

function Get-Survivors {
    param([hashtable]$Before, [hashtable]$After)
    foreach ($procId in $After.Keys) {
        if ($Before.ContainsKey($procId)) { continue }
        if ($procId -eq $PID) { continue }
        $p = $After[$procId]
        $detached = Test-Detached -Table $After -Proc $p
        $interesting = Test-Interesting -Proc $p
        if (-not ($detached -or $interesting)) { continue }
        $tag = if ($detached) { 'DETACHED' } else { 'SURVIVOR' }
        # rel=1 — процесс похож на относящийся к тестам (python/git/go/тест-бинарник/
        # sleep), rel=0 — просто оторвавшийся. На реальном Windows-хосте в отчёт
        # попадают фоновые утилиты самой машины (проверено: LenovoVantage), и без
        # этого флага они неотличимы от настоящей улики. Ничего не скрываем —
        # различать приходится при разборе.
        $rel = if ($interesting) { 1 } else { 0 }
        $cmd = if ($p.Cmd) { $p.Cmd } else { $p.Name }
        if ($cmd.Length -gt 220) { $cmd = $cmd.Substring(0, 220) + '…' }
        Write-Line ("{0} rel={1} pid={2} ppid={3} :: {4}" -f $tag, $rel, $p.Pid, $p.Ppid, $cmd) -Notice:($rel -eq 1)
        Write-Line ("    родословная: {0}" -f (Format-Ancestry -Table $After -Ppid ([int]$p.Ppid)))
        if ($KillLeakedDescendants -and $detached -and $interesting) {
            Stop-ProcessTree -TargetPid $p.Pid
            Write-Line ("    KILLED pid={0} — оторванный потомок не должен пережить шаг (иначе пайпы раннера останутся открытыми)" -f $p.Pid)
        }
    }
}

function Write-CurrentCensus {
    param([string]$Reason)
    Write-Line ("--- процессы на момент '{0}' (кто жив, с родителями) ---" -f $Reason)
    $table = Get-CensusTimed
    foreach ($procId in ($table.Keys | Sort-Object)) {
        $p = $table[$procId]
        if (-not (Test-Interesting -Proc $p)) { continue }
        $cmd = if ($p.Cmd) { $p.Cmd } else { $p.Name }
        if ($cmd.Length -gt 160) { $cmd = $cmd.Substring(0, 160) + '…' }
        Write-Line ("    pid={0} ppid={1} :: {2}" -f $p.Pid, $p.Ppid, $cmd)
    }
}

# --- запуск с пределом и редиректом в файл -------------------------------------
function Stop-ProcessTree {
    param([int]$TargetPid)
    if ($script:IsWin) {
        $psi = New-Object System.Diagnostics.ProcessStartInfo
        $psi.UseShellExecute = $false
        $psi.CreateNoWindow = $true
        $psi.FileName = "$env:SystemRoot\System32\taskkill.exe"
        $psi.Arguments = "/T /F /PID $TargetPid"
        $tk = [System.Diagnostics.Process]::Start($psi)
        if (-not $tk.WaitForExit(20000)) {
            Write-Line "WARN: taskkill не завершился за 20 с (pid $TargetPid)"
        }
        return
    }
    $table = Get-Census
    foreach ($k in $table.Keys) {
        if ([int]$table[$k].Ppid -eq $TargetPid) { Stop-ProcessTree -TargetPid ([int]$k) }
    }
    & kill -9 $TargetPid 2>$null | Out-Null
}

function Start-BoundedCommand {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [string[]]$ArgumentList = @(),
        [Parameter(Mandatory = $true)][string]$LogPath,
        [Parameter(Mandatory = $true)][int]$TimeoutMs,
        [string]$Label = '',
        [string]$WorkDir = $script:RepoDir,
        [string]$StdErrPath = ''
    )
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    if ($WorkDir) { $psi.WorkingDirectory = $WorkDir }
    # Пайпов нет намеренно: см. шапку файла.
    $psi.RedirectStandardOutput = $false
    $psi.RedirectStandardError = $false
    $quoted = ($ArgumentList | ForEach-Object { '"' + ($_ -replace '"', '\"') + '"' }) -join ' '
    if ($StdErrPath) {
        # stderr отдельно там, где его содержимое не парсится (go list): иначе
        # предупреждение «matched no packages» читается как имя пакета.
        if ($script:IsWin) {
            $psi.FileName = $env:ComSpec
            $psi.Arguments = '/c ""{0}" {1} > "{2}" 2> "{3}""' -f $FilePath, $quoted, $LogPath, $StdErrPath
        } else {
            $psi.FileName = '/bin/sh'
            $psi.Arguments = "-c ""exec '{0}' {1} > '{2}' 2> '{3}'""" -f $FilePath, $quoted, $LogPath, $StdErrPath
        }
    } elseif ($script:IsWin) {
        $psi.FileName = $env:ComSpec
        $psi.Arguments = '/c ""{0}" {1} > "{2}" 2>&1"' -f $FilePath, $quoted, $LogPath
    } else {
        $psi.FileName = '/bin/sh'
        $psi.Arguments = "-c ""exec '{0}' {1} > '{2}' 2>&1""" -f $FilePath, $quoted, $LogPath
    }
    $proc = [System.Diagnostics.Process]::Start($psi)
    $null = $proc.Handle   # без этого ExitCode после WaitForExit(ms) может быть пустым
    $exited = $proc.WaitForExit($TimeoutMs)
    if (-not $exited) {
        Write-Line ("TIMEOUT {0} — {1} с, убиваю дерево pid={2}" -f $Label, [int]($TimeoutMs / 1000), $proc.Id)
        Stop-ProcessTree -TargetPid $proc.Id
        [void]$proc.WaitForExit(15000)
    }
    $code = if ($exited) { $proc.ExitCode } else { -1 }
    return [pscustomobject]@{ Exited = $exited; ExitCode = $code; Pid = $proc.Id; LogPath = $LogPath }
}

# --- основной прогон -----------------------------------------------------------
New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
Write-Line ("guard start: host={0} repo={1} budget={2}s perPackage={3}s goTimeout={4}s killLeaks={5}" -f `
    $env:OS, $script:RepoDir, $BudgetSec, $PerPackageSec, $GoTimeoutSec, [bool]$KillLeakedDescendants) -Notice -NoticeText "start host=$env:OS budget=${BudgetSec}s perPkg=${PerPackageSec}s goTimeout=${GoTimeoutSec}s"

# Наша родословная: её нельзя трогать при убийстве оторванных потомков.
$census0 = Get-CensusTimed
$cur = $PID
$guardDepth = 0
while ($guardDepth++ -lt 10 -and $census0.ContainsKey([int]$cur)) {
    $script:Ours[[int]$cur] = $true
    $cur = [int]$census0[[int]$cur].Ppid
}

if (-not (Test-Path (Join-Path $script:RepoDir 'go.mod'))) {
    Write-Line ("FATAL: в '{0}' нет go.mod — рабочий каталог задан неверно (нужен корень репозитория)" -f $script:RepoDir) -Notice
    exit 1
}

$go = 'go'
$listLog = Join-Path $LogDir 'go-list.out.txt'
$listErrLog = Join-Path $LogDir 'go-list.err.txt'
$list = Start-BoundedCommand -FilePath $go -ArgumentList @('list', './...') `
    -LogPath $listLog -StdErrPath $listErrLog -WorkDir $script:RepoDir `
    -TimeoutMs ($ListTimeoutSec * 1000) -Label 'go list ./...'
if (-not $list.Exited -or $list.ExitCode -ne 0) {
    # Ровно тот случай, когда раньше в summary не было НИ ОДНОЙ строки: первая
    # команда шага, без дедлайна, без вывода. Теперь он назван явно.
    Write-Line ("FATAL: go list ./... не отработал (exited={0} rc={1}) — это была первая точка без дедлайна" -f $list.Exited, $list.ExitCode) -Notice
    Get-Content $listLog, $listErrLog -ErrorAction SilentlyContinue | Select-Object -Last 20 | ForEach-Object { Write-Line ("    go list: " + $_) }
    Write-CurrentCensus -Reason 'go list завис'
    exit 1
}
$pkgs = @(Get-Content $listLog -ErrorAction SilentlyContinue | Where-Object { $_ -and $_.Trim() -ne '' -and $_ -notmatch '\s' })
if ($pkgs.Count -eq 0) {
    Write-Line "FATAL: go list не назвал ни одного пакета (см. go-list.out.txt / go-list.err.txt)"
    exit 1
}
Write-Line ("пакетов: {0} :: {1}" -f $pkgs.Count, ($pkgs -join ' '))

$failed = @()
$notChecked = 0
foreach ($pkg in $pkgs) {
    if ([int]((Get-Date) - $script:Start).TotalSeconds -gt $BudgetSec) {
        $notChecked++
        Write-Line ("BUDGET-SKIP {0} — бюджет {1} с исчерпан, пакет НЕ проверен" -f $pkg, $BudgetSec)
        continue
    }
    $safe = ($pkg -replace '[^A-Za-z0-9._-]', '_')
    $pkgLog = Join-Path $LogDir "$safe.txt"
    # Аннотацию на каждую строку НЕ ставим: GitHub держит на шаг ровно 10
    # аннотаций и отбрасывает самые новые (проверено дважды: 21 → 10 и 19 → те
    # же 10, причём терялись хвост с итогом и последние пакеты). 17 строк
    # `=== pkg ===` съели бы весь бюджет и вытеснили бы всё, что важно.
    # В аннотацию идут только: старт, находки rel=1, провалы и итог. Обычно
    # это 2 строки. Полный ход остаётся в логе и в summary.
    Write-Line "=== $pkg ==="
    $before = Get-CensusTimed
    $t0 = Get-Date
    $r = Start-BoundedCommand -FilePath $go `
        -ArgumentList @('test', $pkg, '-count=1', '-timeout', "$($GoTimeoutSec)s") `
        -LogPath $pkgLog -WorkDir $script:RepoDir `
        -TimeoutMs ($PerPackageSec * 1000) -Label "go test $pkg"
    $dur = [int]((Get-Date) - $t0).TotalSeconds
    if ($r.Exited -and $r.ExitCode -eq 0) {
        Write-Line ("ok      {0}  {1}s" -f $pkg, $dur)
    } else {
        if (-not $r.Exited) {
            Write-Line ("FAIL    {0}  {1}s (внешний предел, exit=-1)" -f $pkg, $dur) -Notice
        } else {
            Write-Line ("FAIL    {0}  {1}s (exit={2})" -f $pkg, $dur, $r.ExitCode) -Notice
        }
        Get-Content $pkgLog -ErrorAction SilentlyContinue | Select-Object -Last 25 | ForEach-Object { Write-Line ("    | " + $_) }
        $failed += $pkg
    }
    # Кто остался жить после пакета — это и есть ответ «кто переживает тест».
    Get-Survivors -Before $before -After (Get-CensusTimed)
}

Write-Line "--- итог ---"
Write-Line ("ok={0} fail={1} notChecked={2}" -f ($pkgs.Count - $failed.Count - $notChecked), $failed.Count, $notChecked) -Notice -NoticeText ("CENSUS ok={0} fail={1} notChecked={2}" -f ($pkgs.Count - $failed.Count - $notChecked), $failed.Count, $notChecked)
if ($failed.Count -gt 0) { Write-Line ("провал: " + ($failed -join ' ')) -Notice }
if ($notChecked -gt 0 -or $failed.Count -gt 0) { exit 1 }
exit 0
