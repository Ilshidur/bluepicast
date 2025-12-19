# BluePiCast

**A lightweight web-based toolkit to proxy Snapcast audio to Bluetooth/ALSA/Pipewire.**

**Works on Raspberry Pi 3+, 4, 5, Zero 2 W.**

## Features

- Nice Web UI
- Get your audio stream from a Snapcast server
- Route it automatically to an ALSA/Pipewire device, or to a Bluetooth device using BlueALSA

## Install Script

Install on your existing Raspberry Pi OS with a single command:

```bash
curl -sSL https://raw.githubusercontent.com/Ilshidur/bluepicast/main/install.sh | sudo bash
```

Access the web interface at `http://<raspberry-pi-ip>:8080`

## Requirements

- Raspberry Pi 3, 4, or newer (built-in Bluetooth)
- Raspberry Pi OS (or any Linux with BlueZ)
- BlueZ Bluetooth stack (pre-installed on Raspberry Pi OS)

## License

MIT License - see [LICENSE](LICENSE) for details.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
