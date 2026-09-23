; serialtap Windows 安装包（CI: choco install innosetup; iscc /DMyAppVersion=x.y.z serialtap.iss）
; 产物：serialtap-setup-<版本>.exe。安装后开始菜单/桌面图标直接进托盘
; （serialtap.exe tray —— 自带 FreeConsole，双击不留黑窗）。
; 按用户安装（PrivilegesRequired=lowest → %LOCALAPPDATA%\Programs），无需管理员。

#ifndef MyAppVersion
#define MyAppVersion "0.1.0"
#endif

[Setup]
AppId={{7B5E9C2A-1F4D-4E8B-9A63-2D84F0A51C93}
AppName=serialtap
AppVersion={#MyAppVersion}
AppPublisher=serialtap
AppPublisherURL=https://github.com/mickeyzzc/serialtap
DefaultDirName={localappf}\Programs\serialtap
DefaultGroupName=serialtap
UninstallDisplayIcon={app}\icon.ico
OutputBaseFilename=serialtap-setup-{#MyAppVersion}
SetupIconFile=icon.ico
Compression=lzma2
SolidCompression=yes
ArchitecturesInstallIn64BitMode=x64compatible
PrivilegesRequired=lowest
MinVersion=10.0

[Tasks]
Name: "desktopicon"; Description: "创建桌面图标（serialtap 托盘）"; GroupDescription: "附加任务:"

[Files]
Source: "serialtap.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "icon.ico"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
; 托盘常驻 = 安装后点击图标的入口（菜单里可打开 Web 面板/启动守护）
Name: "{group}\serialtap 托盘"; Filename: "{app}\serialtap.exe"; Parameters: "tray"; WorkingDir: "{app}"; IconFilename: "{app}\icon.ico"
Name: "{group}\卸载 serialtap"; Filename: "{uninstallexe}"
Name: "{autodesktop}\serialtap 托盘"; Filename: "{app}\serialtap.exe"; Parameters: "tray"; WorkingDir: "{app}"; IconFilename: "{app}\icon.ico"; Tasks: desktopicon

[Run]
Filename: "{app}\serialtap.exe"; Parameters: "tray"; WorkingDir: "{app}"; Description: "立即启动 serialtap 托盘"; Flags: nowait postinstall skipifsilent

[UninstallRun]
; 卸载前先退出托盘（否则 exe 被占用无法删除）
Filename: "{cmd}"; Parameters: "/c taskkill /im serialtap.exe /f /t"; Flags: runhidden; RunOnceId: "KillTray"
