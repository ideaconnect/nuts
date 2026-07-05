(function () {
  document.addEventListener('DOMContentLoaded', function () {
    var form = document.getElementById('contact-form');

    if (!form) return;

    form.addEventListener('submit', function (event) {
      var hCaptchaResponse = form.querySelector('textarea[name="h-captcha-response"]');

      if (!hCaptchaResponse || !hCaptchaResponse.value) {
        event.preventDefault();
        alert('Please complete the captcha field.');
      }
    });
  });
}());