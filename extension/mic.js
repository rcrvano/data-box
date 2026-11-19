applyI18n();
navigator.mediaDevices.getUserMedia({ audio: true }).then((s) => {
  s.getTracks().forEach((tr) => tr.stop());
  document.getElementById('s').textContent = t('micOk');
  setTimeout(() => window.close(), 1500);
}).catch((e) => {
  document.getElementById('s').textContent = t('micFail', e.message);
});
