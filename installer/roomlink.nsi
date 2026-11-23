; Room Link — Windows installer (NSIS)
; Bundles the GUI app + the Wintun driver file it needs beside it.
; Build:  makensis installer/roomlink.nsi   (output lands in dist/)

!include "MUI2.nsh"

Name "Room Link"
OutFile "..\dist\RoomLink-Setup-v1-prototype.exe"
InstallDir "$PROGRAMFILES64\Room Link"
RequestExecutionLevel admin
SetCompressor /SOLID lzma

VIProductVersion "1.0.0.0"
VIAddVersionKey "ProductName" "Room Link"
VIAddVersionKey "FileDescription" "Room Link — serverless virtual LAN (v1 prototype)"
VIAddVersionKey "FileVersion" "1.0.0.0"
VIAddVersionKey "LegalCopyright" "JoeMafiaNEO"

!define MUI_FINISHPAGE_RUN "$INSTDIR\svlan.exe"
!define MUI_FINISHPAGE_RUN_TEXT "Run Room Link now"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Section "Room Link" SecMain
  SetOutPath "$INSTDIR"
  File "..\dist\svlan.exe"
  File "..\dist\wintun.dll"
  File "..\dist\WINTUN-LICENSE.txt"

  ; The app must run elevated to create its virtual network adapter.
  WriteRegStr HKLM "Software\Microsoft\Windows NT\CurrentVersion\AppCompatFlags\Layers" \
    "$INSTDIR\svlan.exe" "~ RUNASADMIN"

  ; Start Menu shortcut
  CreateDirectory "$SMPROGRAMS\Room Link"
  CreateShortcut "$SMPROGRAMS\Room Link\Room Link.lnk" "$INSTDIR\svlan.exe"

  ; Add/Remove Programs entry
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RoomLink" \
    "DisplayName" "Room Link"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RoomLink" \
    "DisplayVersion" "1.0-prototype"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RoomLink" \
    "Publisher" "JoeMafiaNEO"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RoomLink" \
    "InstallLocation" "$INSTDIR"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RoomLink" \
    "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RoomLink" \
    "NoModify" 1
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RoomLink" \
    "NoRepair" 1

  WriteUninstaller "$INSTDIR\Uninstall.exe"
SectionEnd

Section "Uninstall"
  Delete "$INSTDIR\svlan.exe"
  Delete "$INSTDIR\wintun.dll"
  Delete "$INSTDIR\WINTUN-LICENSE.txt"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"

  Delete "$SMPROGRAMS\Room Link\Room Link.lnk"
  RMDir "$SMPROGRAMS\Room Link"

  DeleteRegValue HKLM "Software\Microsoft\Windows NT\CurrentVersion\AppCompatFlags\Layers" \
    "$INSTDIR\svlan.exe"
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RoomLink"
SectionEnd
