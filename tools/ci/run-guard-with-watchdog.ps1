# run-guard-with-watchdog.ps1 — внешняя сторожка для windows-test-guard.ps1.
#
# Идея из 7d1ad20 (сторожка снаружи, а не дедлайн внутри того, что может встать)
# сохранена. Что изменено и почему:
#
#  * сторожка запускает рабочий скрипт с редиректом в ФАЙЛЫ, а не через
#    Start-Process -RedirectStandardOutput/-RedirectStandardError. У Start-Process
#    это .NET-пайпы, а Process.WaitForExit() БЕЗ аргумента (в отличие от
#    WaitForExit(ms)) ждёт не только выхода процесса, но и EOF этих пайпов.
#    Значит .NET-слой сам может стать заблокированным читателем уже ПОСЛЕ того,
#    как всё отработало: ветка таймаута (единственная, где печатаются улики) не
#    наступает, потому что с точки зрения сторожки всё успешно, а шаг стоит до
#    отмены джоба и теряет лог. Файловый хендл, который кто-то держит, не
#    блокирует никого.
#  * по истечении бюджета печатается не только хвост логов, но и ТАБЛИЦА
#    ПРОЦЕССОВ с родителями: она отвечает на вопрос «какой процесс переживает
#    тест», а стопка текстовых логов — нет.
#  * у всех ожиданий внутри есть предел.
#
# Всё напечатанное дублируется в GITHUB_STEP_SUMMARY: он переживает отмену
# джоба, в отличие от лога шага и артефакта.

[CmdletBinding()]
param(
    [int]$BudgetSec = 1200,
    [int]$WorkerBudgetSec = -1,   # бюджет самого рабочего скрипта (по умолчанию BudgetSec-60)
    [int]$PerPackageSec = 540,
    [int]$GoTimeoutSec = 480,
    [switch]$KillLeakedDescendants,
    [string]$LogDir = (Join-Path $env:RUNNER_TEMP 'gotest'),
    [string]$GuardPath = '',
    [string]$StepSummary = $env:GITHUB_STEP_SUMMARY
)

$ErrorActionPreference = 'Continue'
$script:IsWin = ($env:OS -eq 'Windows_NT')

# $PSScriptRoot в значениях по умолчанию param() под Windows PowerShell 5.1
# пуст (там ещё не выставлен), поэтому путь к воркеру ищем в теле скрипта.
# Проверено: с `-File` значение по умолчанию давало Join-Path с пустым Path.
if (-not $GuardPath) {
    $here = if ($PSScriptRoot) { $PSScriptRoot } else { Split-Path -Parent $MyInvocation.MyCommand.Path }
    $GuardPath = Join-Path $here 'windows-test-guard.ps1'
}
if (-not (Test-Path -LiteralPath $GuardPath)) {
    throw "рабочий скрипт не найден: $GuardPath"
}

function Write-Both {
    param([string]$Text)
    Write-Host $Text
    if ($StepSummary) { Add-Content -Path $StepSummary -Value $Text -ErrorAction SilentlyContinue }
}

function Ensure-Win32Helpers {
    if (-not $script:IsWin) { return }
    if ('Fidespipe.Win32Guard' -as [type]) { return }
    # Add-Type зовёт компилятор C# и сам по себе предела не имеет, а стоит он
    # ДО старта воркера. Зависни он — шаг не напечатает ни одной строки и не
    # оставит ни одного файла в артефакте, то есть ровно то, что мы наблюдали.
    # Компилировать в постороннем процессе нельзя: тип нужен ЗДЕСЬ, в
    # Get-CensusText. Поэтому предела не добавляем, а делаем зависание
    # видимым — маркер до входа в Add-Type попадает в stdout и в лог.
    if (-not $script:Win32TypeDef) {
        # -TypeDefinition, а не -MemberDefinition: using-директивы легальны только
        # на верхнем уровне исходника. Тип тот же — Fidespipe.Win32Guard.
        $script:Win32TypeDef = @'
            using System;
            using System.Collections.Generic;
            using System.Diagnostics;
            using System.IO;
            using System.Runtime.InteropServices;

            namespace Fidespipe { public static class Win32Guard {

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

            [DllImport("kernel32.dll", SetLastError = true)]
            public static extern IntPtr GetStdHandle(int nStdHandle);

            [DllImport("kernel32.dll", SetLastError = true)]
            public static extern bool SetStdHandle(int nStdHandle, IntPtr hHandle);

            [DllImport("kernel32.dll", SetLastError = true)]
            public static extern bool SetHandleInformation(IntPtr hObject, uint dwMask, uint dwFlags);

            [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
            public static extern IntPtr CreateToolhelp32Snapshot(uint dwFlags, uint th32ProcessID);

            [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
            public static extern bool Process32FirstW(IntPtr hSnapshot, ref PROCESSENTRY32W lppe);

            [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
            public static extern bool Process32NextW(IntPtr hSnapshot, ref PROCESSENTRY32W lppe);

            [DllImport("kernel32.dll", SetLastError = true)]
            public static extern bool CloseHandle(IntPtr hObject);

            [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
            public static extern IntPtr CreateFileW(string name, uint access, uint share,
                IntPtr sec, uint disp, uint flags, IntPtr tmpl);

            // Запускает дочерний процесс так, чтобы CreateProcessW ни через таблицу
            // дескрипторов (bInheritHandles=TRUE), ни через BasepStandardHandleInit
            // (NtDuplicateObject с OBJ_INHERIT, когда STARTF_USESTDHANDLES не задан)
            // не смог продублировать пайпы Runner.Worker в дочерний процесс.
            public static Process StartDetachedFromStdPipes(ProcessStartInfo psi) {
                IntPtr hIn  = GetStdHandle(-10);
                IntPtr hOut = GetStdHandle(-11);
                IntPtr hErr = GetStdHandle(-12);
                foreach (IntPtr h in new IntPtr[] { hIn, hOut, hErr }) {
                    if (h != IntPtr.Zero && h != new IntPtr(-1)) {
                        SetHandleInformation(h, 1, 0);
                    }
                }
                // NUL открываем через CreateFileW напрямую: FileStream("NUL") в
                // .NET Framework (PowerShell 5.1) бросает NotSupportedException,
                // а CreateFileW работает везде.
                const uint RW = 0xC0000000;
                IntPtr hNul = CreateFileW("NUL", RW, 3, IntPtr.Zero, 3, 0, IntPtr.Zero);
                if (hNul == IntPtr.Zero || hNul == new IntPtr(-1)) {
                    return Process.Start(psi); // откат: запуск как раньше
                }
                SetHandleInformation(hNul, 1, 0);
                try {
                    SetStdHandle(-10, hNul);
                    SetStdHandle(-11, hNul);
                    SetStdHandle(-12, hNul);
                    return Process.Start(psi);
                } finally {
                    SetStdHandle(-10, hIn);
                    SetStdHandle(-11, hOut);
                    SetStdHandle(-12, hErr);
                    CloseHandle(hNul);
                }
            }

            // Снимок процессов через ядро (CreateToolhelp32Snapshot) без WMI/DCOM:
            // Get-CimInstance Win32_Process ходит в сервис Winmgmt и ReadProcessMemory
            // чужих PEB, где локальный COM-вызов может зависнуть навсегда мимо
            // -OperationTimeoutSec.
            public static List<ProcEntry> SnapshotProcesses() {
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
    Write-Both 'watchdog: компилирую Win32-помощник (Add-Type, csc) — если зависнем тут, причина найдена'
    try {
        Add-Type -TypeDefinition $script:Win32TypeDef
        Write-Both 'watchdog: компиляция завершилась'
    } catch {
        Write-Both ("watchdog: Add-Type не удался, census пойдёт через Get-Process: {0}" -f $_.Exception.Message)
    }
}

function Kill-Tree {
    param([int]$TargetPid)
    if ($script:IsWin) {
        $psi = New-Object System.Diagnostics.ProcessStartInfo
        $psi.UseShellExecute = $false
        $psi.CreateNoWindow = $true
        $psi.FileName = "$env:SystemRoot\System32\taskkill.exe"
        $psi.Arguments = "/T /F /PID $TargetPid"
        $tk = [System.Diagnostics.Process]::Start($psi)
        if (-not $tk.WaitForExit(15000)) {
            Write-Both "watchdog: WARN taskkill не завершился за 15 с (pid $TargetPid)"
        }
        return
    }
    $kids = @(ps -eo 'pid=,ppid=' | Where-Object { $_ -match '^\s*(\d+)\s+(\d+)$' -and [int]$Matches[2] -eq $TargetPid } |
        ForEach-Object { [int]($_-replace '^\s*(\d+).*', '$1') })
    foreach ($k in $kids) { Kill-Tree -TargetPid $k }
    & kill -9 $TargetPid 2>$null | Out-Null
}

function Get-CensusText {
    if ($script:IsWin) {
        try {
            Ensure-Win32Helpers
            return ([Fidespipe.Win32Guard]::SnapshotProcesses() |
                Sort-Object Pid |
                ForEach-Object { "pid={0} ppid={1} {2}" -f $_.Pid, $_.Ppid, $_.Name })
        } catch {
            return (Get-Process -ErrorAction SilentlyContinue |
                ForEach-Object { "pid={0} {1}" -f $_.Id, $_.ProcessName })
        }
    }
    return (& ps -eo 'pid=,ppid=,etimes=,args=')
}

function Dump-Evidence {
    param([string]$LogDir, [string]$Out, [string]$Err)
    Write-Both "--- census на момент зависания (кто вообще есть, с родителями) ---"
    foreach ($line in (Get-CensusText)) { Write-Both ("    " + $line) }
    foreach ($f in @($Out, $Err)) {
        Write-Both ("--- хвост {0} ---" -f (Split-Path $f -Leaf))
        if (Test-Path $f) { Get-Content $f | Select-Object -Last 60 | ForEach-Object { Write-Both ("    " + $_) } }
    }
    Write-Both "--- пер-пакетные логи (последние 15 строк каждого) ---"
    Get-ChildItem $LogDir -Filter '*.txt' -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -notin @((Split-Path $Out -Leaf), (Split-Path $Err -Leaf)) } |
        ForEach-Object {
            Write-Both ("### {0} ({1} байт, изменён {2})" -f $_.Name, $_.Length, $_.LastWriteTime.ToString('HH:mm:ss'))
            Get-Content $_.FullName -Tail 15 -ErrorAction SilentlyContinue | ForEach-Object { Write-Both ("    " + $_) }
        }
}

if ($WorkerBudgetSec -lt 0) { $WorkerBudgetSec = [Math]::Max(30, $BudgetSec - 60) }
New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
$out = Join-Path $LogDir 'guard.out.txt'
$err = Join-Path $LogDir 'guard.err.txt'

$hostExe = (Get-Process -Id $PID).Path
$killArg = if ($KillLeakedDescendants) { ' -KillLeakedDescendants' } else { '' }
Ensure-Win32Helpers
Write-Both ("watchdog: host=$hostExe budget=${BudgetSec}s workerBudget=${WorkerBudgetSec}s guard=$GuardPath")

# Рабочий скрипт — отдельный процесс, вывод уходит в ФАЙЛЫ (никаких пайпов).
$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.UseShellExecute = $false
$psi.CreateNoWindow = $true
$psi.RedirectStandardOutput = $false
$psi.RedirectStandardError = $false
if ($script:IsWin) {
    # Редирект ОБЯЗАН идти через cmd.exe. Проверено на этой машине: при
    # FileName = pwsh.exe и `> out` прямо в Arguments оператор редиректа НЕ
    # выполняется — он не является частью синтаксиса argv, молча
    # проглатывается, вывод уходит в унаследованный stdout родителя, а
    # выход равен 0. Т.е. заявленное в шапке «пайпов нет» оказывалось бы
    # неправдой, а Dump-Evidence читал бы несуществующие файлы.
    $psi.FileName = $env:ComSpec
    $psi.Arguments = '/c ""{0}" -NoProfile -ExecutionPolicy Bypass -File "{1}" -LogDir "{2}" -BudgetSec {3} -PerPackageSec {4} -GoTimeoutSec {5}{6} > "{7}" 2> "{8}""' -f `
        $hostExe, $GuardPath, $LogDir, $WorkerBudgetSec, $PerPackageSec, $GoTimeoutSec, $killArg, $out, $err
    $worker = [Fidespipe.Win32Guard]::StartDetachedFromStdPipes($psi)
} else {
    # Ветка только для прогона сторожки вне Windows (проверка логики).
    $psi.FileName = '/bin/sh'
    $psi.Arguments = "-c ""exec '{0}' -NoProfile -File '{1}' -LogDir '{2}' -BudgetSec {3} -PerPackageSec {4} -GoTimeoutSec {5}{6} > '{7}' 2> '{8}'""" -f `
        $hostExe, $GuardPath, $LogDir, $WorkerBudgetSec, $PerPackageSec, $GoTimeoutSec, $killArg, $out, $err
    $worker = [System.Diagnostics.Process]::Start($psi)
}
# Handle кэшируется до чтения ExitCode: без этого свойство может быть пустым
# (та же причина, что и у Start-Process -PassThru в 10d78eb).
$null = $worker.Handle
Write-Both ("watchdog: worker pid={0}" -f $worker.Id)

if (-not $worker.WaitForExit($BudgetSec * 1000)) {
    Write-Both ("watchdog: worker не уложился в {0} с — убиваю дерево и снимаю улики" -f $BudgetSec)
    # Сначала быстрый снимок без WMI (Get-Process не виснет на заблокированном
    # процессе), затем убийство дерева, и только потом — полный дамп улик.
    # Раньше Dump-Evidence шёл ДО Kill-Tree и вызывал Get-CimInstance по живому
    # зависшему дереву, рискуя повиснуть в DCOM до того, как worker будет убит.
    Get-Process -ErrorAction SilentlyContinue | ForEach-Object {
        Write-Both ("    pre-kill: pid={0} {1}" -f $_.Id, $_.ProcessName)
    }
    Kill-Tree -TargetPid $worker.Id
    [void]$worker.WaitForExit(15000)
    if (-not $worker.HasExited) { Write-Both "watchdog: дерево worker'а всё ещё живо после taskkill" }
    Dump-Evidence -LogDir $LogDir -Out $out -Err $err
    exit 1
}
# Успешная ветка: читаем ФАЙЛЫ. Пайпов у нас нет вообще, поэтому ждать EOF тут
# нечего: ни WaitForExit(ms), ни ExitCode не могут быть заблокированы чужим
# потомком, который удержал бы перенаправленный поток.
if (Test-Path $out) { Get-Content $out | ForEach-Object { Write-Host $_ } }
if (Test-Path $err) { Get-Content $err | ForEach-Object { Write-Host $_ } }
if ($worker.ExitCode -ne 0) { Write-Both ("watchdog: worker exit={0}" -f $worker.ExitCode); exit 1 }
exit 0
