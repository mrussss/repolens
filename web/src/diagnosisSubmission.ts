export function getStableDiagnosisIdempotencyKey(
  payloadFingerprint: string,
  storage: Pick<Storage, 'getItem' | 'setItem'>,
  createKey: () => string,
): string {
  const savedPayload = storage.getItem('repolens-diagnosis-payload');
  let key = storage.getItem('repolens-diagnosis-key') || '';
  if (savedPayload !== payloadFingerprint || !key) {
    key = createKey();
    storage.setItem('repolens-diagnosis-payload', payloadFingerprint);
    storage.setItem('repolens-diagnosis-key', key);
  }
  return key;
}
