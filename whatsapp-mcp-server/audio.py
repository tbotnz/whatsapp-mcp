import os
import subprocess
import tempfile


def convert_to_opus_ogg(input_file, output_file=None, bitrate="32k", sample_rate=24000):
    """
    Convert an audio file to Opus format in an Ogg container.

    Args:
        input_file (str): Path to the input audio file
        output_file (str, optional): Path to save the output file. If None, replaces the
                                    extension of input_file with .ogg
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)

    Returns:
        str: Path to the converted file

    Raises:
        FileNotFoundError: If the input file doesn't exist
        RuntimeError: If the ffmpeg conversion fails
    """
    if not os.path.isfile(input_file):
        raise FileNotFoundError(f"Input file not found: {input_file}")

    if output_file is None:
        output_file = os.path.splitext(input_file)[0] + ".ogg"

    output_dir = os.path.dirname(output_file)
    if output_dir and not os.path.exists(output_dir):
        os.makedirs(output_dir, mode=0o700)

    # "file:" prefixes stop ffmpeg from interpreting a path as a protocol URL
    # (e.g. "concat:", "pipe:", "http:") and "-nostdin" keeps it from reading
    # the MCP server's stdin. Arguments are passed as a list, never a shell.
    cmd = [
        "ffmpeg",
        "-nostdin",
        "-hide_banner",
        "-loglevel", "error",
        "-i", "file:" + input_file,
        "-c:a", "libopus",
        "-b:a", bitrate,
        "-ar", str(sample_rate),
        "-application", "voip",
        "-vbr", "on",
        "-compression_level", "10",
        "-frame_duration", "60",
        "-y",
        "file:" + output_file,
    ]

    try:
        subprocess.run(
            cmd,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
            check=True,
            timeout=300,
        )
        return output_file
    except subprocess.TimeoutExpired:
        raise RuntimeError("Audio conversion timed out")
    except FileNotFoundError:
        raise RuntimeError("ffmpeg is not installed or not on PATH")
    except subprocess.CalledProcessError as e:
        raise RuntimeError(f"Failed to convert audio. You likely need to install ffmpeg {e.stderr}")


def convert_to_opus_ogg_temp(input_file, bitrate="32k", sample_rate=24000, output_dir=None):
    """
    Convert an audio file to Opus format in an Ogg container and store in a temporary file.

    Args:
        input_file (str): Path to the input audio file
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)
        output_dir (str, optional): Directory for the temporary file. Defaults to the
                                    system temp directory.

    Returns:
        str: Path to the temporary file with the converted audio

    Raises:
        FileNotFoundError: If the input file doesn't exist
        RuntimeError: If the ffmpeg conversion fails
    """
    temp_file = tempfile.NamedTemporaryFile(suffix=".ogg", delete=False, dir=output_dir)
    temp_file.close()

    try:
        convert_to_opus_ogg(input_file, temp_file.name, bitrate, sample_rate)
        return temp_file.name
    except Exception:
        if os.path.exists(temp_file.name):
            os.unlink(temp_file.name)
        raise


if __name__ == "__main__":
    import sys

    if len(sys.argv) < 2:
        print("Usage: python audio.py input_file [output_file]")
        sys.exit(1)

    input_file = sys.argv[1]

    try:
        result = convert_to_opus_ogg_temp(input_file)
        print(f"Successfully converted to: {result}")
    except Exception as e:
        print(f"Error: {e}")
        sys.exit(1)
