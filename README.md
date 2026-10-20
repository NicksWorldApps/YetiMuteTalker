# Yeti Mute Talker

**Hear whether your Blue Yeti is muted, without having to see it.**

Yeti Mute Talker is a small, free Windows program for people who own a **Blue Yeti microphone**. Every time you press the mute button on the front of the mic, it tells you out loud whether the mic is now muted or live.

The Yeti shows its mute state only with a light on the mic, which is easy to miss and impossible to see if you're blind. Miss it and you end up with a silent recording. If you use a screen reader, sit away from your mic, or just don't want to check the light before every recording or stream, this program is for you.

## Download

Get **YetiMuteTalker-1.0-windows.zip** from the [Releases](../../releases) page. It contains a single program, **YetiMuteTalker.exe**. There is nothing to install.

## What it does

- Press the Yeti's mute button and you hear:
  - two **falling** beeps, then "Microphone muted"
  - two **rising** beeps, then "Microphone live"
- It works from any app, even while you're recording, streaming or gaming.
- It tells you when the mic is unplugged or plugged back in.
- Hotkeys let you check the mic any time without touching it.
- It speaks with the built-in Windows voice, so it works alongside screen readers like NVDA, JAWS and Narrator.
- It has no window. It runs quietly in the notification area (system tray), with a menu that works from the keyboard.

## Who it's for

This program is made for **Blue Yeti microphones**, and it has been tested with the **Yeti Stereo Microphone**. On these mics the mute button works inside the microphone, and Windows is never told that the mic is muted. That's why the usual Windows tools can't tell you the mute state, and why this program exists.

Other USB microphones with a hardware mute button may work too, because the program listens for the mic going silent. Only the Yeti has been tested.

## Requirements

- Windows 10 or Windows 11, 64-bit
- A Blue Yeti microphone plugged in over USB

## Getting started

1. Download the ZIP from [Releases](../../releases) and unzip it.
2. Put **YetiMuteTalker.exe** somewhere permanent, such as your Documents folder.
3. Run **YetiMuteTalker.exe**. Windows may warn that the program is unrecognized, because it isn't signed. Choose **More info**, then **Run anyway**.
4. The first time it runs, a short spoken setup of about 10 seconds teaches the program what your mic sounds like:
   - Stay quiet while it listens.
   - When it asks, press the mute button on your Yeti once, then stay quiet again.
   - It says "Done" and tells you whether the mic is muted or live.
5. That's it. Press the Yeti's mute button and it announces the change.

## Hotkeys

These work from any app.

| Keys | What it does |
| --- | --- |
| Ctrl + Alt + M | Say whether the mic is muted right now |
| Ctrl + Alt + L | Say the current sound level (for troubleshooting) |
| Ctrl + Alt + C | Run the setup again |
| Ctrl + Alt + Shift + M | Close Yeti Mute Talker |

## Tray menu

Press **Windows + B** to move to the notification area, find Yeti Mute Talker with the arrow keys (it may be under "Show hidden icons"), and press **Enter**. The menu lets you:

- hear the microphone status
- hear the sound level
- run the setup again
- turn the beeps on or off
- make it **start automatically with Windows**
- open the settings folder
- exit

Running YetiMuteTalker.exe again while it's already running doesn't start a second copy. It just tells you the current mic status.

## How it works

The Yeti's mute button doesn't tell Windows anything, so Yeti Mute Talker listens to the microphone instead. A live mic always picks up a tiny bit of room sound, while a muted Yeti sends complete silence. The program watches for that difference.

- When there has been complete silence for a short moment (0.4 seconds by default), the mic is muted.
- When steady sound comes through for about half a second, the mic is live again. A bump, a touch or moving the mic is too short to count, so it won't trick the program into saying live.

The setup step measures your own mic and room, so the program knows where the line between muted and live falls.

**Privacy:** the program only measures how loud the sound is. Nothing is recorded, saved or sent anywhere. While it runs, Windows will show that an app is using your microphone. That's expected.

## Settings

Settings are kept in `%APPDATA%\YetiMuteTalker\settings.ini`. Use **Open settings folder** in the tray menu to get there, and open the file in Notepad. You can change:

- the words it says for muted and live
- the speech speed
- whether it plays the beeps
- how long the mic must be silent to count as muted
- how long sound must last to count as live again
- which microphone it watches (it looks for a mic with "Yeti" in its name)

Restart Yeti Mute Talker after editing.

## Troubleshooting

- **It says muted when the mic is live.** Make sure Windows lets desktop apps use the microphone: **Settings > Privacy & security > Microphone**.
- **It misses button presses, or announces the wrong state.** Run the setup again with **Ctrl + Alt + C**, and stay as quiet as you can. Do the same if you turn the Yeti's gain knob a lot.
- **A hotkey doesn't work.** Another program may already be using it.
- **Something else is wrong.** Look in **YetiMuteTalker.log** in the settings folder, and include what it says when you open an issue.

## Building from source

The program is written in Go, and the source is in this repository.

```
go mod tidy
GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui -s -w" -o YetiMuteTalker.exe .
```

`rsrc_windows_amd64.syso` holds the program's icon and manifest. It was made from `icon.ico` and `app.manifest` with [rsrc](https://github.com/akavel/rsrc).

## License

Released under the MIT License. See [LICENSE](LICENSE).

Yeti Mute Talker is an independent project. It is not made by, affiliated with, or endorsed by Logitech or Blue Microphones. "Blue Yeti" is a trademark of its owner.
