# 🌸 Vrushie 🌸

A cute and simple file server that serves files once or to a limited number of clients with an adorable terminal interface!

## ✨ Features

- 🎯 **Simple & Intuitive**: No complex setup - just point to a file and serve!
- 🔒 **Secure by Default**: Serves files once and shuts down automatically.
- 🎨 **Beautiful TUI**: Gorgeous terminal interface with real-time activity logs.
- 🌐 **Public IP Detection**: Automatically detects your public IP for easy sharing over the internet.
- 📱 **QR Code Display**: Shows a QR code for your public URL for easy mobile downloads.
- 🗜️ **Archive Streaming**: Serve multiple files or entire directories as a `.zip` or `.tar.gz` on the fly.
- 🎛️ **Flexible Access Control**: Serve to specific IPs or limit to N unique clients.
- 💝 **Zero Dependencies**: Single executable, no installation required.

## 🚀 Quick Start

### Basic Usage
```bash
# Serve a file once to the first downloader
vrushie document.pdf

# Serve a whole directory as a .zip archive
vrushie -z ./my_folder/

# Serve multiple files as a .tar.gz archive
vrushie -z --format tar.gz file1.txt image.jpg

# Serve to the first 3 unique IPs on a specific port
vrushie -n 3 -port 8080 video.mp4

# Only allow specific IP addresses
vrushie -ips "192.168.1.10,192.168.1.20" secret-file.zip
```

### Get Help
```bash
vrushie -h          # Short help
vrushie --help      # Detailed help
vrushie -v          # Version info
vrushie --version   # Version info
```

## 📖 Command Line Options

| Flag | Short | Description | Default |
|------|-------|-------------|---------|
| `--help` | `-h` | Show help message | - |
| `--version` | `-v` | Show version information | - |
| `--port` | - | Port to listen on (0 for random) | `0` |
| `--n` | `-n` | Number of downloads allowed | `1` |
| `--ips` | - | Comma-separated list of allowed IPs | - |
| `-z` | | Serve multiple files/a directory as an archive | `false` |
| `--format` | | Archive format to use with -z (zip or tar.gz) | `zip` |


## 🎯 Usage Examples

### 📁 Serve a Single File
```bash
vrushie my-file.pdf
```
Perfect for sharing files quickly - serves to the first person who downloads it, then shuts down.

### 🗜️ Serve an Entire Directory as a Zip
```bash
vrushie -z ./my-project/
```
This will package the entire `my-project` directory into an `archive.zip` and serve it.

### 🗜️ Serve Multiple Files as a Tarball
```bash
vrushie -z --format tar.gz report.docx chart.xlsx
```
This will create an `archive.tar.gz` containing the two specified files.

### 👥 Serve to Multiple People
```bash
vrushie -n 5 presentation.pptx
```
Allows up to 5 unique IP addresses to download the file.

### 🔐 Restrict to Specific IPs
```bash
vrushie -ips "192.168.1.100,192.168.1.101" confidential.docx
```
Only the specified IP addresses can access the file.

### 🌐 Custom Port
```bash
vrushie -port 3000 website.zip
```
Serves on port 3000 instead of a random port.

## 🎨 Interface Preview

When you run vrushie, you'll see a beautiful terminal interface. It automatically detects your public IP for easy sharing!

```
╭───────────────────────────────────────────────────────────────────────────╮
│🌸 Vrushie Server 🌸                                                      │
│                                                                           │
│Serving File: my-file.zip                                                  │
│Size: 9.2 MiB                                                              │
│                                                                           │
│Server Ready! ✨✨                                                        │
│Listening on:                                                              │
│🌐 http://123.45.67.89:59438/   <-- Your Public IP!                        │
│   http://192.168.1.10:59438/                                              │
│   http://127.0.0.1:59438/                                                 │
│                                                                           │
│Access Mode: Serve once to first successful download                       │
│                                                                           │
│Activity Log:                                                              │
│  No activity yet...                                                       │
│                                                                           │
│                                                                           │
│Press 'q' or Ctrl+C to shut down manually.                                 │
╰───────────────────────────────────────────────────────────────────────────╯
```
*On wide terminals, a QR code will be displayed for easy scanning with a mobile device!*

## 🛠️ Installation

### Download Binary
1. Download the latest release from the [releases page](https://github.com/aarmn/vrushie/releases)
2. Make it executable: `chmod +x vrushie`
3. Run it: `./vrushie your-file.txt`

### Build from Source
```bash
git clone https://github.com/aarmn/vrushie
cd vrushie
go build -o vrushie
```

## 🎪 Advanced Features

### 🔄 Access Modes

**Serve Once (Default)**
- Serves to the first successful downloader
- Automatically shuts down after completion
- Perfect for one-time file sharing

**Serve to N Unique IPs**
- Use `-n <number>` to allow multiple unique IP addresses
- Each IP can download once
- Shuts down after N successful downloads

**IP Whitelist**
- Use `-ips "ip1,ip2,ip3"` to restrict access
- Only specified IPs can connect
- Combines with `-n` for additional control

### 🌐 Network Detection
Vrushie automatically detects all available network interfaces and displays URLs for:
- Local network interfaces (WiFi, Ethernet)
- Virtual interfaces (Docker, VPN)
- Localhost (127.0.0.1)

### 🎯 Smart Port Selection
- Default: Random available port (prevents conflicts)
- Custom: Specify with `-port <number>`
- Displays actual port in the interface

## ✅ Todo

- [ ] **RAR Support**: Add the ability to create and serve `.rar` archives.
- [ ] **Password Protection**: Add an optional flag to protect archives with a user-provided password.
- [ ] **Advanced Flag Handling**: Investigate making `-z` an optionally valued flag (e.g., `-z zip`).
- [ ] **Storage Options**: Add a flag to select between streaming from RAM (current) or using a temporary folder for large archives.
- [ ] **URL Helpers**: Add features like URL shortening or better guidance for users behind a NAT. (Push based)
- [ ] **Host as folder**: Host a folder recursively!
- [ ] **Time limit**: Limit the time of hosting, after which, the server will self-destruct (hard and soft mode, to not self destruct while a user is downloading, if time is given, and no user count, default to time only, infinite user)
- [ ] **Select IPs**: Share on specific IPs with interactive selection of endpoints, instead of all end-points
- [ ] **End with Download**: Ensure the program quits at the same time as the last user download ends
- [ ] Show the progress of download if possible

## 🤝 Contributing

We love contributions! Here's how you can help make Vrushie even cuter:

1. 🍴 Fork the repository
2. 🌱 Create a feature branch
3. ✨ Add your improvements
4. 🧪 Test thoroughly
5. 📝 Submit a pull request

## 📜 License

MIT License - feel free to use Vrushie in your projects!

## 💖 Why Vrushie?

Sometimes you just need to share a file quickly without setting up complex servers or dealing with cloud uploads. Vrushie makes file sharing as simple as pointing to a file and watching the magic happen in your terminal!

Perfect for:
- 📊 Sharing presentations in meetings
- 📸 Sending photos to friends on the same network
- 📁 Distributing files during workshops
- 🎮 Sharing game files with teammates
- 📚 Distributing course materials

---

Made with 💝 and ✨ as AARMN The Limitless by Gemini 2.5 pro and Claude 4 Sonnet! (and *Jules* now)