; 在仓库根目录执行 ISCC /DMyAppVersion=x.y.z /DBinaryDir=... deploy\windows\linkgate.iss
#ifndef MyAppVersion
  #define MyAppVersion "0.0.0"
#endif
#ifndef BinaryDir
  #define BinaryDir "..\..\bin\windows-setup"
#endif

[Setup]
AppId={{A50359EB-9D85-4AFA-9113-A36D12C825EC}
AppName=Linkgate
AppVersion={#MyAppVersion}
AppPublisher=Linkgate
DefaultDirName={localappdata}\Linkgate
DisableDirPage=yes
UsePreviousAppDir=no
DefaultGroupName=Linkgate
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
DisableProgramGroupPage=yes
DisableWelcomePage=no
WizardStyle=modern
LicenseFile=..\..\LICENSE
SetupIconFile=..\..\cmd\linkgate-tray\icons\tray-win.ico
UninstallDisplayIcon={app}\linkgate-tray.exe
OutputDir=..\..\dist\windows-setup
OutputBaseFilename=Linkgate-Setup-{#MyAppVersion}-win64
Compression=lzma2
SolidCompression=yes
CloseApplications=no
RestartApplications=no
SetupLogging=yes

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Messages]
ButtonBack=< 上一步(&B)
ButtonNext=下一步(&N) >
ButtonInstall=安装(&I)
ButtonFinish=完成(&F)
ButtonCancel=取消
WelcomeLabel1=欢迎使用 [name] 安装向导
WelcomeLabel2=本向导将为当前用户安装 [name/ver]。%n%n默认内核在后台运行；系统策略不允许 S4U 时，会明确提示启用可见窗口兼容模式。升级会停止本安装的内核与托盘；请先完成正在进行的请求。配置、令牌和日志不会被覆盖。
WizardSelectDir=选择安装位置
SelectDirDesc=将 [name] 安装在哪里？
SelectDirLabel3=点击“下一步”继续，或点击“浏览”选择其他目录。
WizardSelectTasks=选择附加任务
SelectTasksDesc=需要安装哪些启动项？
SelectTasksLabel2=勾选需要的选项，然后点击“下一步”。
WizardReady=准备安装
ReadyLabel1=已准备好在你的计算机上安装 [name]。
ReadyLabel2a=点击“安装”继续，或点击“上一步”修改设置。
WizardInstalling=正在安装
InstallingLabel=正在安装 [name]，请稍候。
FinishedHeadingLabel=[name] 安装完成
FinishedLabelNoIcons=Linkgate 已安装。点击“完成”启动后台内核、托盘并打开面板，然后添加你自己的服务与线路。
FinishedLabel=Linkgate 已安装。点击“完成”启动后台内核、托盘并打开面板，然后添加你自己的服务与线路。

[Tasks]
Name: "autostart"; Description: "开机自启（当前用户登录后启动）"; Flags: checkedonce
Name: "tray"; Description: "创建托盘（常驻系统通知区域）"; Flags: checkedonce

[Files]
Source: "{#BinaryDir}\linkgate.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BinaryDir}\linkgate-tray.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "common.ps1"; DestDir: "{app}"; Flags: ignoreversion
Source: "transaction.ps1"; DestDir: "{app}"; Flags: ignoreversion
Source: "update.ps1"; DestDir: "{app}"; Flags: ignoreversion
Source: "update.bat"; DestDir: "{app}"; Flags: ignoreversion
Source: "check-update.bat"; DestDir: "{app}"; Flags: ignoreversion
Source: "setup-helper.ps1"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\Linkgate 托盘"; Filename: "{app}\linkgate-tray.exe"; Parameters: "{code:TrayArguments}"; WorkingDir: "{app}"; Check: CreateTrayIcon
Name: "{group}\卸载 Linkgate"; Filename: "{uninstallexe}"

[Run]
Filename: "{sys}\WindowsPowerShell\v1.0\powershell.exe"; Parameters: "{code:SilentLaunchParameters}"; Flags: runhidden waituntilterminated; Check: WizardSilent; AfterInstall: CheckSilentLaunchResult
Filename: "{sys}\WindowsPowerShell\v1.0\powershell.exe"; Parameters: "{code:LaunchParameters}"; Description: "立即启动 Linkgate 并打开面板"; Flags: postinstall runhidden waituntilterminated skipifsilent; AfterInstall: CheckLaunchResult

; 卸载只由 usUninstall 事件执行：需要检查返回码，并支持 EXE 缺失时的 helper 回退。

[Code]
var
  ErrorFile: String;
  SnapshotDir: String;
  CommitAttempted: Boolean;
  InstallationCompleted: Boolean;
  SilentLaunchFailed: Boolean;
  CompatibilityWarningShown: Boolean;

function TaskListSelection(Tasks, Task: String; Selected: Boolean): Boolean;
var
  Token: String;
  Separator: Integer;
begin
  Tasks := Trim(Tasks);
  if Length(Tasks) >= 2 then
    if (Tasks[1] = '"') and (Tasks[Length(Tasks)] = '"') then
      Tasks := Copy(Tasks, 2, Length(Tasks) - 2);
  while Tasks <> '' do begin
    Separator := Pos(',', Tasks);
    if Separator = 0 then Separator := Length(Tasks) + 1;
    Token := Lowercase(Trim(Copy(Tasks, 1, Separator - 1)));
    Delete(Tasks, 1, Separator);
    if (Token = Lowercase(Task)) or (Token = '*' + Lowercase(Task)) then Selected := True;
    if Token = '!' + Lowercase(Task) then Selected := False;
  end;
  Result := Selected;
end;

function SilentTaskSelected(Task: String): Boolean;
var
  I: Integer;
  Argument, Tasks, MergeTasks: String;
  HasTasks: Boolean;
begin
  { 静默模式没有任务页，checkedonce 不能作为 WizardIsTaskSelected 的默认值来源。 }
  HasTasks := False;
  Tasks := '';
  MergeTasks := '';
  for I := 1 to ParamCount do begin
    Argument := ParamStr(I);
    if Pos('/TASKS=', Uppercase(Argument)) = 1 then begin
      HasTasks := True;
      Tasks := Copy(Argument, 8, Length(Argument));
    end;
    if Pos('/MERGETASKS=', Uppercase(Argument)) = 1 then
      MergeTasks := Copy(Argument, 13, Length(Argument));
  end;
  Result := (Lowercase(Task) = 'autostart') or (Lowercase(Task) = 'tray');
  if HasTasks then Result := TaskListSelection(Tasks, Task, False);
  Result := TaskListSelection(MergeTasks, Task, Result);
end;

function SelectedBoolean(Task: String): String;
var
  Selected: Boolean;
begin
  if WizardSilent then Selected := SilentTaskSelected(Task)
  else Selected := WizardIsTaskSelected(Task);
  if Selected then Result := 'true' else Result := 'false';
end;

function CreateTrayIcon: Boolean;
begin
  Result := SelectedBoolean('tray') = 'true';
end;

function HelperParameters(Phase, HelperPath: String): String;
begin
  Result := '-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + HelperPath +
    '" -Phase ' + Phase + ' -InstallDir "' + ExpandConstant('{app}') +
    '" -Autostart ' + SelectedBoolean('autostart') + ' -WithTray ' + SelectedBoolean('tray') +
    ' -ErrorFile "' + ErrorFile + '" -SnapshotDir "' + SnapshotDir +
    '" -SourceDir "' + ExpandConstant('{tmp}') + '"';
end;

function PersistentErrorPath(TemporaryPath: String): String;
var
  Name, Alternative: String;
begin
  Name := ExtractFileName(ExtractFileDir(TemporaryPath)) + '-' + ExtractFileName(TemporaryPath);
  Result := ExpandConstant('{userpf}\.linkgate\install-errors\') + Name;
  Alternative := ExpandConstant('{app}\logs\') + Name;
  if not FileExists(Result) and FileExists(Alternative) then Result := Alternative;
end;

procedure RecordHelperDiagnostic(TemporaryPath: String);
var
  Lines: TArrayOfString;
  I: Integer;
  Path: String;
begin
  Path := PersistentErrorPath(TemporaryPath);
  Log('Linkgate 持久诊断：' + Path);
  if not LoadStringsFromFile(Path, Lines) then
    if not LoadStringsFromFile(TemporaryPath, Lines) then Exit;
  for I := 0 to GetArrayLength(Lines) - 1 do Log(Lines[I]);
end;

procedure CheckRegistrationWarning;
var
  Path: String;
begin
  Path := PersistentErrorPath(ErrorFile + '.warning.log');
  if FileExists(Path) then begin
    RecordHelperDiagnostic(ErrorFile + '.warning.log');
    if not WizardSilent and not CompatibilityWarningShown then begin
      MsgBox('兼容模式：S4U 不可用，内核会以可见命令行窗口运行；关闭该窗口会停止内核。托盘仍独立运行。详情：' + Path, mbInformation, MB_OK);
      CompatibilityWarningShown := True;
    end;
  end;
end;

function RunHelper(Phase, HelperPath: String): Boolean;
var
  ExitCode: Integer;
begin
  Result := Exec(ExpandConstant('{sys}\WindowsPowerShell\v1.0\powershell.exe'),
    HelperParameters(Phase, HelperPath), '', SW_HIDE, ewWaitUntilTerminated, ExitCode);
  if Result then Result := ExitCode = 0;
  if not Result then RecordHelperDiagnostic(ErrorFile);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := '';
  ErrorFile := ExpandConstant('{tmp}\linkgate-prepare-error.log');
  SnapshotDir := ExpandConstant('{localappdata}\Linkgate\backups\setup-') +
    GetDateTimeString('yyyymmdd-hhnnss-zzz', '-', ':');
  while DirExists(SnapshotDir) do SnapshotDir := SnapshotDir + '-next';
  ExtractTemporaryFile('setup-helper.ps1');
  ExtractTemporaryFile('common.ps1');
  ExtractTemporaryFile('transaction.ps1');
  ExtractTemporaryFile('linkgate.exe');
  ExtractTemporaryFile('linkgate-tray.exe');
  if not RunHelper('Prepare', ExpandConstant('{tmp}\setup-helper.ps1')) then
    Result := '安装前检查失败：请退出旧 Linkgate 或下载目录中的便携实例，确认任务属于当前安装目录后重试。错误详情：' + PersistentErrorPath(ErrorFile);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssInstall then begin
    { ssInstall 在自动复制前且异常致命；helper 显式复制 EXE、配置并负责失败回滚。 }
    CommitAttempted := True;
    ErrorFile := ExpandConstant('{tmp}\linkgate-configure-error.log');
    if not RunHelper('Configure', ExpandConstant('{tmp}\setup-helper.ps1')) then
      RaiseException('安装提交失败，安装已中止。恢复结果及完整原文见持久错误日志：' + PersistentErrorPath(ErrorFile));
    CheckRegistrationWarning;
  end;
  if CurStep = ssPostInstall then InstallationCompleted := True;
end;

procedure DeinitializeSetup;
begin
  if CommitAttempted and not InstallationCompleted then begin
    ErrorFile := ExpandConstant('{tmp}\linkgate-rollback-error.log');
    if not RunHelper('Rollback', ExpandConstant('{tmp}\setup-helper.ps1')) then
      MsgBox('安装未完成，自动恢复失败。请保留快照：' + SnapshotDir + '。持久错误日志：' + PersistentErrorPath(ErrorFile), mbError, MB_OK);
  end;
end;

function TrayArguments(Param: String): String;
var
  ConfigPath: String;
begin
  ConfigPath := GetIniString('Install', 'Config', ExpandConstant('{userpf}\.linkgate\config.yaml'), SnapshotDir + '\config.ini');
  Result := '-config "' + ConfigPath + '"';
end;

function LaunchParameters(Param: String): String;
begin
  ErrorFile := ExpandConstant('{tmp}\linkgate-launch-error.log');
  Result := HelperParameters('Launch', ExpandConstant('{app}\setup-helper.ps1')) + ' -OpenPanel true';
end;

function SilentLaunchParameters(Param: String): String;
begin
  ErrorFile := ExpandConstant('{tmp}\linkgate-silent-launch-error.log');
  Result := HelperParameters('Launch', ExpandConstant('{app}\setup-helper.ps1')) + ' -OpenPanel false';
end;

procedure CheckSilentLaunchResult;
begin
  SilentLaunchFailed := FileExists(ErrorFile) or FileExists(PersistentErrorPath(ErrorFile));
  if SilentLaunchFailed then RecordHelperDiagnostic(ErrorFile)
  else CheckRegistrationWarning;
end;

function GetCustomSetupExitCode: Integer;
begin
  if SilentLaunchFailed then Result := 1 else Result := 0;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  ExitCode: Integer;
  Started: Boolean;
  Params: String;
begin
  if CurUninstallStep = usUninstall then begin
    { 唯一卸载入口，异常致命；不再有第二次 [UninstallRun] 调用。 }
    if FileExists(ExpandConstant('{app}\linkgate.exe')) then
      Started := Exec(ExpandConstant('{app}\linkgate.exe'), '-uninstall', '', SW_HIDE, ewWaitUntilTerminated, ExitCode)
    else begin
      ErrorFile := ExpandConstant('{tmp}\linkgate-uninstall-error.log');
      Params := '-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' +
        ExpandConstant('{app}\setup-helper.ps1') + '" -Phase Uninstall -InstallDir "' +
        ExpandConstant('{app}') + '" -ErrorFile "' + ErrorFile + '"';
      Started := Exec(ExpandConstant('{sys}\WindowsPowerShell\v1.0\powershell.exe'), Params,
        '', SW_HIDE, ewWaitUntilTerminated, ExitCode);
    end;
    if not Started then RaiseException('无法清理 Linkgate 启动项，尚未删除安装文件。请检查安装目录后重试。');
    if ExitCode <> 0 then RaiseException('Linkgate 启动项清理失败，尚未删除安装文件。请检查任务所属用户、目录或错误日志后重试。');
  end;
end;

procedure CheckLaunchResult;
begin
  if FileExists(ErrorFile) or FileExists(PersistentErrorPath(ErrorFile)) then begin
    RecordHelperDiagnostic(ErrorFile);
    MsgBox('Linkgate 首次启动失败。请检查配置端口与用户目录下 .linkgate\kernel.log 后重试。错误详情：' + PersistentErrorPath(ErrorFile), mbError, MB_OK);
  end else CheckRegistrationWarning;
end;
