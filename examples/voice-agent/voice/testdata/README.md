`speech.ogg` contains the same 80 recorded synthetic 20 ms Opus packets as
`pkg/callharness/testdata/tts-opus-20ms.packets`, rewrapped with the example's
Pion writer. It retains a 312-sample pre-skip to exercise the provider reader.
No live request, private speech or API credential is needed by tests.
