async function copyToClipboard(button, value) {
  if (!value) return;
  await navigator.clipboard.writeText(value);
  const original = button.textContent;
  button.textContent = "Copied";
  window.setTimeout(() => {
    button.textContent = original;
  }, 1200);
}

document.querySelectorAll("[data-copy-value]").forEach((button) => {
  button.addEventListener("click", () => {
    copyToClipboard(button, button.getAttribute("data-copy-value"));
  });
});

document.querySelectorAll(".code-block__copy").forEach((button) => {
  button.addEventListener("click", () => {
    const pre = button.parentElement.querySelector("pre");
    copyToClipboard(button, pre ? pre.innerText : "");
  });
});
