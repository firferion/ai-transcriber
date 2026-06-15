Add-Type -AssemblyName System.Drawing
$img = [System.Drawing.Image]::FromFile("icon.png")
$bmp = New-Object System.Drawing.Bitmap($img, 256, 256)
$bmp.Save("winres/icon.png", [System.Drawing.Imaging.ImageFormat]::Png)
$bmp.Dispose()
$img.Dispose()
