"""Independent chrony evidence; never adjusts the host clock."""
import math
import re
import subprocess
import time


def require(ok, message):
    if not ok:
        raise ValueError(message)


def clock_evidence():
    output = subprocess.run(['/usr/bin/chronyc', 'tracking'], capture_output=True, text=True, timeout=5, check=True).stdout
    require('Leap status' in output and re.search(r'Leap status\s*:\s*Normal', output), 'Unsynchronised host clock')
    def number(label):
        m = re.search(re.escape(label) + r'\s*:\s*([-+0-9.eE]+)', output)
        require(m is not None, 'Missing chrony tracking field')
        value = float(m.group(1))
        require(math.isfinite(value), 'Invalid chrony tracking value')
        return value
    error = abs(number('System time')) + abs(number('Root dispersion')) + abs(number('Root delay')) / 2
    return {'tracking': output, 'errorBoundNs': int(math.ceil(error * 1e9)),
            'rateErrorPpm': abs(number('Residual freq')) + abs(number('Skew')), 'measuredUnixNs': str(time.time_ns())}

